// Package openaicompat fala com qualquer servidor compatível com a API da
// OpenAI: Ollama (/v1), vLLM, LiteLLM, a própria OpenAI, DashScope (Qwen)...
//
// É um repasse: troca o "model" pelo ID real, força stream=false (o gateway
// faz o streaming para o cliente) e devolve a resposta.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

type Config struct {
	// BaseURL até o /v1, ex: http://ollama:11434/v1
	BaseURL string
	// APIKey opcional (Authorization: Bearer). Vem de variável de ambiente,
	// nunca do arquivo de config.
	APIKey string
	// Timeout por chamada. Padrão 120s (modelo local em CPU é lento).
	Timeout time.Duration
}

type Provider struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Provider {
	if cfg.Timeout == 0 {
		cfg.Timeout = 120 * time.Second
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Provider{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

// send monta o corpo (cópia do JSON do agente com model/stream trocados) e
// faz o POST. Status >= 300 já vira *provider.Error aqui, ANTES de qualquer
// byte ir para o cliente — é isso que permite fallback também no streaming.
func (p *Provider) send(ctx context.Context, c provider.Call, stream bool) (*http.Response, error) {
	body := make(map[string]any, len(c.Raw)+1)
	for k, v := range c.Raw {
		body[k] = v
	}
	body["model"] = c.Model
	body["stream"] = stream
	delete(body, "stream_options")
	if stream {
		// Sempre pedimos usage ao upstream (para as métricas). Se o agente
		// também pediu, o gateway devolve; senão fica só com a gente.
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if c.Request.EffectiveMaxTokens() == 0 && c.DefaultMaxTokens > 0 {
		body["max_tokens"] = c.DefaultMaxTokens
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, provider.Errorf(http.StatusBadRequest, "invalid_request", "serializando pedido: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	if tp := c.Header.Get("traceparent"); tp != "" {
		req.Header.Set("traceparent", tp)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, provider.Errorf(http.StatusBadGateway, "upstream_unreachable", "%v", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, provider.Errorf(resp.StatusCode, "upstream_error", "%s", truncate(string(raw), 500))
	}
	return resp, nil
}

func (p *Provider) Chat(ctx context.Context, c provider.Call) (*openai.ChatResponse, error) {
	resp, err := p.send(ctx, c, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, provider.Errorf(http.StatusBadGateway, "upstream_read", "%v", err)
	}
	var out openai.ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "resposta não é JSON de chat: %v", err)
	}
	if len(out.Choices) == 0 {
		return nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "resposta sem choices")
	}
	return &out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s… (%d bytes)", s[:n], len(s))
}
