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
	"github.com/chwiee/nautilm/internal/live"
	"github.com/chwiee/nautilm/internal/mask"
	"github.com/chwiee/nautilm/internal/metrics"
	"github.com/chwiee/nautilm/internal/quota"
	"github.com/chwiee/nautilm/internal/secrets"
	"github.com/chwiee/nautilm/internal/store"
	"github.com/chwiee/nautilm/internal/usage"
	"github.com/chwiee/nautilm/internal/valkey"
	"github.com/chwiee/nautilm/internal/webauth"
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

	providers, err := live.BuildAll(ctx, cfg, live.DefaultBuilder)
	if err != nil {
		return err
	}
	// Database (tokens from the dashboard, sessions, audit): optional.
	var st store.Store
	var stored *auth.StoredTokens
	if cfg.Database.Enabled() {
		st, err = openDatabase(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer st.Close()
		stored = auth.NewStoredTokens(st)
		stored.OnError = func(err error) { log.Error("stored tokens: database error", "error", err) }
	}

	authn, err := buildAuth(cfg, stored, log)
	if err != nil {
		return err
	}

	gw := gateway.New(cfg, providers, authn, metrics.New(), log)
	var cfgMgr *live.Manager
	if st != nil {
		gw.UseStore(st, stored)
		// Config tab: providers, aliases, prices and groups changed in the
		// dashboard, on top of this file. Replicas pick changes up by polling.
		cfgMgr = &live.Manager{Base: cfg, BaseProviders: providers, Store: st, Secrets: &secrets.AWSReader{},
			Build: live.DefaultBuilder, Log: log}
		cfgMgr.Start(ctx)
		go cfgMgr.Run(ctx)
		gw.UseConfigManager(cfgMgr)
	}
	if e := cfg.Dashboard.Entra; e != nil {
		wc := webauth.Config{Issuer: e.Issuer(), TenantID: e.TenantID, ClientID: e.ClientID,
			ClientSecret: e.ClientSecret, PublicURL: cfg.Dashboard.PublicURL, AdminGroups: e.AdminGroups,
			ViewerGroups: e.ViewerGroups, SessionTTL: cfg.Dashboard.SessionTTL, CheckTenant: true}
		if cfgMgr != nil {
			wc.Groups = func() ([]string, []string) {
				in := cfgMgr.Current().Cfg.Dashboard.Entra
				return in.AdminGroups, in.ViewerGroups
			}
		}
		gw.UseEntra(webauth.New(wc, st, log))
		log.Info("dashboard sign-in", "provider", "entra", "issuer", e.Issuer(), "public_url", cfg.Dashboard.PublicURL,
			"admin_groups", len(e.AdminGroups), "viewer_groups", len(e.ViewerGroups))
	}
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

// openDatabase resolves the URL (env or Secrets Manager) and connects, with
// retries: a pod that starts before Postgres must wait, not crash-loop.
func openDatabase(ctx context.Context, cfg *config.Config, log *slog.Logger) (*store.Postgres, error) {
	url, err := secrets.DatabaseURL(ctx, cfg.Database.URL, cfg.Database.AWSSecretARN, cfg.Database.AWSSecretKey, nil)
	if err != nil {
		return nil, err
	}
	wait := 2 * time.Second
	deadline := time.Now().Add(5 * time.Minute)
	for {
		p, err := store.OpenPostgres(ctx, url)
		if err == nil {
			source := "url"
			if cfg.Database.AWSSecretARN != "" {
				source = "aws_secret"
			}
			log.Info("database connected", "source", source)
			return p, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("database: %w", err)
		}
		log.Warn("database not ready, retrying", "error", err, "in", wait.String())
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		wait = min(wait*2, 30*time.Second)
	}
}

func buildAuth(cfg *config.Config, stored *auth.StoredTokens, log *slog.Logger) (auth.Authenticator, error) {
	var chain auth.Chain
	if stored != nil {
		// First: only "naut_..." tokens are looked up; anything else passes on.
		chain = append(chain, stored)
	}
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
