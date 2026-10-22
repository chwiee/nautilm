// pedagio é o LLM Gateway: todo agente fala com ele no formato OpenAI, e só
// ele tem credencial para o Bedrock (ou outro provedor).
//
//	pedagio -config config.yaml
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

	"github.com/chwiee/pedagio/internal/auth"
	"github.com/chwiee/pedagio/internal/config"
	"github.com/chwiee/pedagio/internal/gateway"
	"github.com/chwiee/pedagio/internal/mask"
	"github.com/chwiee/pedagio/internal/metrics"
	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/internal/provider/bedrock"
	"github.com/chwiee/pedagio/internal/provider/openaicompat"
	"github.com/chwiee/pedagio/internal/quota"
)

func main() {
	path := flag.String("config", envOr("PEDAGIO_CONFIG", "config.yaml"), "arquivo de configuração")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*path, log); err != nil {
		log.Error("pedagio parou", "error", err)
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
	quotaStore := "memória"
	if cfg.Cotas.Desligado {
		quotaStore = "desligado"
	} else if cfg.Cotas.ValkeyURL != "" {
		vk, err := quota.NewValkey(cfg.Cotas.ValkeyURL)
		if err != nil {
			return fmt.Errorf("cotas.valkey_url: %w", err)
		}
		// Valkey fora na subida NÃO impede o pedagio de subir: as cotas
		// deixam passar até ele voltar (fail-open), e o log avisa.
		if err := vk.Ping(ctx); err != nil {
			log.Warn("cotas: Valkey indisponível agora; cotas deixam passar até ele voltar", "error", err)
		}
		gw.UseQuotaCounter(vk)
		quotaStore = "valkey"
	}
	pm, pd := cfg.Limits("*")
	mk, _ := mask.New(cfg.Mascarar)
	log.Info("proteções", "mascarar", mk.Enabled(), "cotas", quotaStore,
		"cota_padrao_minuto", pm, "cota_padrao_dia", pd)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Sem WriteTimeout: resposta de LLM pode demorar minutos. O limite
		// fica no timeout de cada provider.
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("pedagio no ar", "listen", cfg.Listen, "aliases", len(cfg.Models), "providers", len(providers), "auth", cfg.Auth.Modes)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Desligamento gracioso: deixa as chamadas em andamento terminarem.
	log.Info("desligando")
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
			log.Warn("auth.modes contém \"none\": qualquer um pode chamar o pedagio. Só para desenvolvimento!")
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
