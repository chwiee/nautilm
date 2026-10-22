package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chwiee/pedagio/internal/auth"
	"github.com/chwiee/pedagio/internal/config"
	"github.com/chwiee/pedagio/internal/metrics"
	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

// spy guarda o que o provider RECEBEU: é isso que o modelo veria.
type spy struct {
	call  provider.Call
	input []string
}

func (s *spy) Chat(_ context.Context, c provider.Call) (*openai.ChatResponse, error) {
	s.call = c
	txt := "ok"
	return &openai.ChatResponse{Choices: []openai.Choice{{Message: openai.ResponseMessage{Role: "assistant", Content: &txt}}}}, nil
}

func (s *spy) Embed(_ context.Context, c provider.EmbedCall) ([][]float32, *openai.Usage, error) {
	s.input = append([]string(nil), c.Input...)
	out := make([][]float32, len(c.Input))
	for i := range out {
		out[i] = []float32{1}
	}
	return out, &openai.Usage{}, nil
}

func setupSpy(t *testing.T, extra string) (*httptest.Server, *spy, *metrics.Registry) {
	t.Helper()
	cfg, err := config.Parse([]byte(embedConfig + extra))
	if err != nil {
		t.Fatal(err)
	}
	s := &spy{}
	m := metrics.New()
	gw := New(cfg, map[string]provider.Provider{"a": s}, auth.Chain{auth.NewTokens(cfg.Auth.Tokens)}, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return srv, s, m
}

const chatComSegredos = `{"model":"smart","messages":[
 {"role":"system","content":"você ajuda o time"},
 {"role":"user","content":[{"type":"text","text":"meu cpf é 123.456.789-09 e a senha=hunter2"}]},
 {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"logs","arguments":"{\"token\":\"abc123secret\"}"}}]},
 {"role":"tool","tool_call_id":"c1","content":"conectando com AKIAABCDEFGHIJKLMNOP em ana@empresa.com"}
],"tools":[{"type":"function","function":{"name":"logs","parameters":{"type":"object","properties":{"token":{"type":"string"}}}}}]}`

func TestChatMascaraAntesDoProvider(t *testing.T) {
	srv, s, m := setupSpy(t, "")
	resp := postPath(t, srv, "/v1/chat/completions", chatComSegredos)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}

	// 1) O que vai para o provider openaicompat (mapa cru)
	raw, _ := json.Marshal(s.call.Raw)
	// 2) O que vai para o Bedrock (struct)
	st, _ := json.Marshal(s.call.Request.Messages)
	for name, got := range map[string]string{"raw": string(raw), "struct": string(st)} {
		for _, leak := range []string{"123.456.789-09", "hunter2", "abc123secret", "AKIAABCDEFGHIJKLMNOP", "ana@empresa.com"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: vazou %q", name, leak)
			}
		}
		for _, mark := range []string{"[CPF]", "[SEGREDO]", "[AWS_KEY]", "[EMAIL]"} {
			if !strings.Contains(got, mark) {
				t.Errorf("%s: faltou %s em %s", name, mark, got)
			}
		}
	}
	// Estrutura intacta: tool_call_id, definição da tool e o parâmetro "token".
	if s.call.Request.Messages[3].ToolCallID != "c1" || len(s.call.Request.Tools) != 1 {
		t.Errorf("estrutura mudou: %+v", s.call.Request.Messages[3])
	}
	if !strings.Contains(string(mustJSON(s.call.Raw["tools"])), `"token":{"type":"string"}`) {
		t.Error("definição da tool não pode ser mascarada")
	}
	// Agente fica sabendo, métrica conta (sem o valor!).
	if h := resp.Header.Get("X-Pedagio-Masked"); h != "aws_key=1,cpf=1,email=1,segredo=2" {
		t.Errorf("header = %q", h)
	}
	var buf bytes.Buffer
	m.Write(&buf)
	if !strings.Contains(buf.String(), `pedagio_masked_total{agent="k",rule="segredo"} 2`) {
		t.Errorf("métrica:\n%s", buf.String())
	}
}

func TestChatSemDadoSensivelNaoMexe(t *testing.T) {
	srv, s, _ := setupSpy(t, "")
	resp := postPath(t, srv, "/v1/chat/completions", `{"model":"smart","messages":[{"role":"user","content":"pod checkout em CrashLoopBackOff"}]}`)
	if resp.Header.Get("X-Pedagio-Masked") != "" || s.call.Request.Messages[0].Content.Text != "pod checkout em CrashLoopBackOff" {
		t.Errorf("não devia mexer: %q", s.call.Request.Messages[0].Content.Text)
	}
}

func TestEmbeddingsMascara(t *testing.T) {
	srv, s, _ := setupSpy(t, "")
	postPath(t, srv, "/v1/embeddings", `{"model":"embed","input":["cliente 123.456.789-09","normal"]}`)
	if s.input[0] != "cliente [CPF]" || s.input[1] != "normal" {
		t.Errorf("input = %q", s.input)
	}
}

func TestMascararAjustavel(t *testing.T) {
	srv, s, _ := setupSpy(t, "mascarar: {desligar: [email], extra: [{nome: matricula, regex: 'MAT-\\d{6}'}]}\n")
	postPath(t, srv, "/v1/chat/completions", `{"model":"smart","messages":[{"role":"user","content":"ana@x.com MAT-123456"}]}`)
	if got := s.call.Request.Messages[0].Content.Text; got != "ana@x.com [MATRICULA]" {
		t.Errorf("got %q", got)
	}

	srv, s, _ = setupSpy(t, "mascarar: {desligado: true}\n")
	postPath(t, srv, "/v1/chat/completions", `{"model":"smart","messages":[{"role":"user","content":"123.456.789-09"}]}`)
	if got := s.call.Request.Messages[0].Content.Text; got != "123.456.789-09" {
		t.Errorf("desligado mexeu: %q", got)
	}

	if _, err := config.Parse([]byte(embedConfig + "mascarar: {desligar: [cpff]}\n")); err == nil || !strings.Contains(err.Error(), "cpff") {
		t.Errorf("erro de config deveria citar a regra: %v", err)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
