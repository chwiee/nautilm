package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chwiee/pedagio/internal/auth"
	"github.com/chwiee/pedagio/internal/config"
	"github.com/chwiee/pedagio/internal/metrics"
	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

type fakeEmbedder struct {
	fake
	err  error
	dims int
}

func (f *fakeEmbedder) Embed(_ context.Context, c provider.EmbedCall) ([][]float32, *openai.Usage, error) {
	f.calls = append(f.calls, c.Model)
	if f.err != nil {
		return nil, nil, f.err
	}
	f.dims = c.Dimensions
	out := make([][]float32, len(c.Input))
	for i := range c.Input {
		out[i] = []float32{float32(i), 1}
	}
	return out, &openai.Usage{PromptTokens: 3 * len(c.Input), TotalTokens: 3 * len(c.Input)}, nil
}

const embedConfig = `
providers:
  a: {type: openai, base_url: http://x}
models:
  smart: {provider: a, model: chat-model}
  embed: {type: embedding, provider: a, model: embed-model, dimensions: 512}
auth: {modes: [tokens], tokens: {k: tok}}
agents: {k: {models: ["*"]}}
`

func setupEmbed(t *testing.T, e *fakeEmbedder) *httptest.Server {
	t.Helper()
	cfg, err := config.Parse([]byte(embedConfig))
	if err != nil {
		t.Fatal(err)
	}
	gw := New(cfg, map[string]provider.Provider{"a": e}, auth.Chain{auth.NewTokens(cfg.Auth.Tokens)}, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbeddings(t *testing.T) {
	e := &fakeEmbedder{}
	srv := setupEmbed(t, e)
	resp := postPath(t, srv, "/v1/embeddings", `{"model":"embed","input":["a","b"]}`)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var out openai.EmbeddingResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Data) != 2 || out.Data[1].Index != 1 || out.Model != "embed" || out.Usage.PromptTokens != 6 {
		t.Errorf("resposta: %+v", out)
	}
	if e.calls[0] != "embed-model" || e.dims != 512 {
		t.Errorf("chamada: %v dims=%d", e.calls, e.dims)
	}
	// input string única também vale
	if resp := postPath(t, srv, "/v1/embeddings", `{"model":"embed","input":"só um"}`); resp.StatusCode != 200 {
		t.Errorf("input string: %d", resp.StatusCode)
	}
}

func TestEmbeddingsTipoErrado(t *testing.T) {
	srv := setupEmbed(t, &fakeEmbedder{})
	if r := postPath(t, srv, "/v1/embeddings", `{"model":"smart","input":["a"]}`); r.StatusCode != 400 {
		t.Errorf("chat em /v1/embeddings: %d", r.StatusCode)
	}
	if r := postPath(t, srv, "/v1/chat/completions", `{"model":"embed","messages":[{"role":"user","content":"oi"}]}`); r.StatusCode != 400 {
		t.Errorf("embedding em /v1/chat/completions: %d", r.StatusCode)
	}
}

func TestEmbeddingSemFallbackNoConfig(t *testing.T) {
	_, err := config.Parse([]byte(`
providers: {a: {type: openai, base_url: http://x}}
models:
  e1: {type: embedding, provider: a, model: m1, fallback: [e2]}
  e2: {type: embedding, provider: a, model: m2}
`))
	if err == nil {
		t.Fatal("embedding com fallback deveria ser recusado no config")
	}
}

func postPath(t *testing.T, srv *httptest.Server, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
