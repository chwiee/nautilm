// nautilm (Nautilus LLM Gateway): every agent talks to it in the OpenAI
// format, and only it holds credentials for Bedrock (or any other provider).
// It also keeps the ledger of who used which model, where, and what it cost.
//
//	nautilm -config config.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chwiee/nautilm/internal/auth"
	"github.com/chwiee/nautilm/internal/config"
	"github.com/chwiee/nautilm/internal/gateway"
	"github.com/chwiee/nautilm/internal/mask"
	"github.com/chwiee/nautilm/internal/metrics"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/internal/provider/bedrock"
	"github.com/chwiee/nautilm/internal/provider/openaicompat"
	"github.com/chwiee/nautilm/internal/quota"
	"github.com/chwiee/nautilm/internal/usage"
	"github.com/chwiee/nautilm/internal/valkey"
)

func main() {
	path := flag.String("config", envOr("NAUTILM_CONFIG", "config.yaml"), "config file")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*path, log); err != nil {
		log.Error("nautilm stopped", "error", err)
		os.Exit(1)
	}
}

func run(path string, log *slog.Logger) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	providers, err := buildProviders(ctx, cfg)
	if err != nil {
		return err
	}
	authn, err := buildAuth(cfg, log)
	if err != nil {
		return err
	}

	gw := gateway.New(cfg, providers, authn, metrics.New(), log)
	store := "memory"
	if cfg.ValkeyURL != "" {
		vk, err := valkey.Connect(cfg.ValkeyURL)
		if err != nil {
			return fmt.Errorf("valkey_url: %w", err)
		}
		defer vk.Close()
		// Valkey down at startup does NOT stop nautilm: quotas let calls
		// through and usage records go to the log until it is back.
		if err := valkey.Ping(ctx, vk); err != nil {
			log.Warn("valkey unavailable right now; quotas fail open and usage records are logged until it is back", "error", err)
		}
		gw.UseQuotaCounter(quota.NewValkey(vk))
		gw.UseUsageStore(usage.NewValkey(vk, time.Duration(cfg.Usage.RetentionDays)*24*time.Hour))
		store = "valkey"
	}

	pm, pd := cfg.Limits("*")
	mk, _ := mask.New(cfg.Masking)
	log.Info("protections", "masking", mk.Enabled(), "quotas", !cfg.Quotas.Disabled, "default_tokens_per_minute", pm,
		"default_tokens_per_day", pd, "store", store)
	log.Info("billing", "usage_ledger", !cfg.Usage.Disabled, "retention_days", cfg.Usage.RetentionDays,
		"price_entries", len(cfg.Pricing), "usage_api", cfg.Dashboard.Token != "")
	// A model without a price is not silently zero: warn loudly at startup.
	if missing := cfg.MissingPrices(time.Now()); len(missing) > 0 {
		log.Warn("models without a price: their calls are recorded as UNPRICED (cost unknown); add them to pricing or set strict_pricing",
			"models", missing)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: an LLM answer may take minutes. The limit is each
		// provider's timeout.
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("nautilm up", "listen", cfg.Listen, "aliases", len(cfg.Models), "providers", len(providers), "auth", cfg.Auth.Modes)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Graceful shutdown: let in-flight calls finish.
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func buildProviders(ctx context.Context, cfg *config.Config) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	for name, p := range cfg.Providers {
		switch p.Type {
		case "bedrock":
			b, err := bedrock.New(ctx, bedrock.Config{Region: p.Region})
			if err != nil {
				return nil, fmt.Errorf("provider %s: %w", name, err)
			}
			out[name] = b
		case "openai":
			out[name] = openaicompat.New(openaicompat.Config{BaseURL: p.BaseURL, APIKey: p.APIKey, Timeout: p.Timeout})
		}
	}
	return out, nil
}

func buildAuth(cfg *config.Config, log *slog.Logger) (auth.Authenticator, error) {
	var chain auth.Chain
	for _, mode := range cfg.Auth.Modes {
		switch mode {
		case "kubernetes":
			k, err := auth.NewKubernetesInCluster(cfg.Auth.Audience)
			if err != nil {
				return nil, err
			}
			chain = append(chain, k)
		case "tokens":
			chain = append(chain, auth.NewTokens(cfg.Auth.Tokens))
		case "none":
			log.Warn(`auth.modes contains "none": anyone can call nautilm. Development only!`)
			chain = append(chain, auth.None{})
		}
	}
	return chain, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
