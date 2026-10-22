package gateway

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/chwiee/pedagio/internal/auth"
	"github.com/chwiee/pedagio/internal/config"
	"github.com/chwiee/pedagio/internal/metrics"
	"github.com/chwiee/pedagio/internal/provider"
)

func setupCfg(t *testing.T, yaml string, a, b *fake) (*httptest.Server, *metrics.Registry) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	gw := New(cfg, map[string]provider.Provider{"a": a, "b": b}, auth.Chain{auth.NewTokens(cfg.Auth.Tokens)}, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return srv, m
}
