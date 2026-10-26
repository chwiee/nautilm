package gateway

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chwiee/nautilm/internal/auth"
	"github.com/chwiee/nautilm/internal/config"
	"github.com/chwiee/nautilm/internal/metrics"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// testConfig: provider "a" serves smart (priced), provider "b" serves fast
// (priced) and has an unpriced model reachable through "cheap".
const testConfig = `
providers:
  a: {type: openai, base_url: http://x}
  b: {type: openai, base_url: http://y}
models:
  smart: {provider: a, model: big-model, fallback: [fast]}
  fast:  {provider: b, model: small-model}
  cheap: {provider: b, model: mystery-model}
  embed: {type: embedding, provider: a, model: embed-model, dimensions: 512}
pricing:
  - {provider: a, model: big-model, input: 3, output: 15}
  - {provider: b, model: small-model, input: 0.5, output: 1}
  - {provider: a, model: embed-model, input: 0.02}
auth:
  modes: [tokens]
  tokens: {k8s: tok-k8s, cli: tok-cli, maestro: tok-maestro, mid: tok-mid}
agents:
  k8s:     {models: [smart, fast, cheap, embed], team: ce, cost_center: CC-1}
  mid:     {models: [fast], team: ce, cost_center: CC-1}
  maestro: {models: [fast], team: platform, on_behalf_of: true}
  "*":     {models: [fast]}
dashboard: {token: dash-secret}
`

// fake is a fake provider: returns err if set, otherwise "ok from <model>"
// with 10 input / 3 output tokens (and the reported cost, if set).
type fake struct {
	err      error
	calls    []string
	reported *float64
}

func (f *fake) Chat(_ context.Context, c provider.Call) (*openai.ChatResponse, error) {
	f.calls = append(f.calls, c.Model)
	if f.err != nil {
		return nil, f.err
	}
	s := "ok from " + c.Model
	return &openai.ChatResponse{
		Object: "chat.completion", Model: c.Model,
		Choices: []openai.Choice{{Message: openai.ResponseMessage{Role: "assistant", Content: &s}, FinishReason: "stop"}},
		Usage:   &openai.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13, Cost: f.reported},
	}, nil
}

type testEnv struct {
	srv     *httptest.Server
	metrics *metrics.Registry
	gw      *Gateway
}

// setupCfg builds a gateway with the given YAML and providers by name.
func setupCfg(t *testing.T, yaml string, providers map[string]provider.Provider) testEnv {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	gw := New(cfg, providers, auth.Chain{auth.NewTokens(cfg.Auth.Tokens)}, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return testEnv{srv: srv, metrics: m, gw: gw}
}

func setup(t *testing.T, a, b provider.Provider) testEnv {
	return setupCfg(t, testConfig, map[string]provider.Provider{"a": a, "b": b})
}

func do(t *testing.T, method, url, token, body string, headers ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// post sends a chat request.
func post(t *testing.T, srv *httptest.Server, token, body string, headers ...string) *http.Response {
	t.Helper()
	return do(t, http.MethodPost, srv.URL+"/v1/chat/completions", token, body, headers...)
}

func metricsText(m *metrics.Registry) string {
	var sb strings.Builder
	m.Write(&sb)
	return sb.String()
}

// readSSE returns the "data:" payloads in order.
func readSSE(t *testing.T, r io.Reader) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, d)
		}
	}
	return out
}

const hello = `{"model":"%s","messages":[{"role":"user","content":"hi"}]}`
