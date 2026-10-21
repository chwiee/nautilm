package gateway

import (
	"bufio"
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

// fake é um provider de mentira: devolve err se houver, senão responde "ok de <model>".
type fake struct {
	err   error
	calls []string
}

func (f *fake) Chat(_ context.Context, c provider.Call) (*openai.ChatResponse, error) {
	f.calls = append(f.calls, c.Model)
	if f.err != nil {
		return nil, f.err
	}
	s := "ok de " + c.Model
	return &openai.ChatResponse{
		Object: "chat.completion", Model: c.Model,
		Choices: []openai.Choice{{Message: openai.ResponseMessage{Role: "assistant", Content: &s}, FinishReason: "stop"}},
		Usage:   &openai.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13},
	}, nil
}

const testConfig = `
providers:
  a: {type: openai, base_url: http://x}
  b: {type: openai, base_url: http://y}
models:
  smart: {provider: a, model: modelo-grande, fallback: [fast]}
  fast:  {provider: b, model: modelo-pequeno}
auth:
  modes: [tokens]
  tokens: {kubets: tok-kubets, cli: tok-cli}
agents:
  kubets: {models: [smart, fast]}
  "*":    {models: [fast]}
`

func setup(t *testing.T, a, b *fake) (*httptest.Server, *metrics.Registry) {
	t.Helper()
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	gw := New(cfg, map[string]provider.Provider{"a": a, "b": b}, auth.Chain{auth.NewTokens(cfg.Auth.Tokens)}, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return srv, m
}

func post(t *testing.T, srv *httptest.Server, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestChatResolveApelido(t *testing.T) {
	a, b := &fake{}, &fake{}
	srv, m := setup(t, a, b)
	resp := post(t, srv, "tok-kubets", `{"model":"smart","messages":[{"role":"user","content":"oi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out openai.ChatResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Model != "smart" {
		t.Errorf("agente deve ver o apelido, viu %q", out.Model)
	}
	if got := resp.Header.Get("X-Pedagio-Model"); got != "modelo-grande" {
		t.Errorf("X-Pedagio-Model = %q", got)
	}
	if len(a.calls) != 1 || a.calls[0] != "modelo-grande" {
		t.Errorf("provider a chamado com %v", a.calls)
	}
	var sb strings.Builder
	m.Write(&sb)
	if !strings.Contains(sb.String(), `pedagio_tokens_total{agent="kubets",alias="smart",model="modelo-grande",kind="input"} 10`) {
		t.Errorf("métrica de tokens ausente:\n%s", sb.String())
	}
}

func TestFallbackSoEmErroRetryable(t *testing.T) {
	// 503 no "smart" → tenta "fast".
	a, b := &fake{err: provider.Errorf(503, "x", "fora")}, &fake{}
	srv, _ := setup(t, a, b)
	resp := post(t, srv, "tok-kubets", `{"model":"smart","messages":[{"role":"user","content":"oi"}]}`)
	if resp.StatusCode != 200 || resp.Header.Get("X-Pedagio-Alias") != "fast" {
		t.Fatalf("esperava fallback para fast: status %d alias %q", resp.StatusCode, resp.Header.Get("X-Pedagio-Alias"))
	}

	// 400 no "smart" → NÃO tenta "fast" (o pedido é que está errado).
	a2, b2 := &fake{err: provider.Errorf(400, "ValidationException", "ruim")}, &fake{}
	srv2, _ := setup(t, a2, b2)
	resp = post(t, srv2, "tok-kubets", `{"model":"smart","messages":[{"role":"user","content":"oi"}]}`)
	if resp.StatusCode != 400 || len(b2.calls) != 0 {
		t.Fatalf("400 não deveria cair no fallback: status %d, chamadas fast %v", resp.StatusCode, b2.calls)
	}
}

func TestPermissoes(t *testing.T) {
	srv, _ := setup(t, &fake{}, &fake{})
	cases := []struct {
		name, token, model string
		want               int
	}{
		{"sem token", "", "fast", 401},
		{"token errado", "nada", "fast", 401},
		{"curinga pode fast", "tok-cli", "fast", 200},
		{"curinga não pode smart", "tok-cli", "smart", 403},
		{"ID cru de modelo é recusado", "tok-kubets", "modelo-grande", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := post(t, srv, c.token, `{"model":"`+c.model+`","messages":[{"role":"user","content":"oi"}]}`)
			if resp.StatusCode != c.want {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status %d, quer %d: %s", resp.StatusCode, c.want, b)
			}
		})
	}
}

func TestStreamSimulado(t *testing.T) {
	srv, _ := setup(t, &fake{}, &fake{})
	resp := post(t, srv, "tok-cli", `{"model":"fast","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"oi"}]}`)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	var content string
	var sawUsage, sawDone bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if line == "" {
			continue
		}
		if line == "[DONE]" {
			sawDone = true
			continue
		}
		var c openai.ChatChunk
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("chunk inválido %q: %v", line, err)
		}
		for _, ch := range c.Choices {
			content += ch.Delta.Content
		}
		if c.Usage != nil {
			sawUsage = true
		}
	}
	if content != "ok de modelo-pequeno" || !sawUsage || !sawDone {
		t.Errorf("content=%q usage=%v done=%v", content, sawUsage, sawDone)
	}
}

func TestModelsListaSoPermitidos(t *testing.T) {
	srv, _ := setup(t, &fake{}, &fake{})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer tok-cli")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list openai.ModelList
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Data) != 1 || list.Data[0].ID != "fast" {
		t.Errorf("cli deveria ver só fast: %+v", list.Data)
	}
}
