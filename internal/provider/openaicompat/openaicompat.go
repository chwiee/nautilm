// Package openaicompat talks to any OpenAI-compatible server: Ollama (/v1),
// vLLM, LiteLLM, OpenAI itself, OpenRouter, DashScope (Qwen)...
//
// It is a pass-through: it swaps "model" for the real ID, controls "stream"
// (the gateway streams to the client) and returns the response. If the
// upstream reports the cost in usage.cost (OpenRouter does), it is kept and
// wins over the nautilm price table.
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

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

type Config struct {
	// BaseURL up to /v1, e.g. http://ollama:11434/v1
	BaseURL string
	// APIKey is optional (Authorization: Bearer). It comes from an environment
	// variable, never from the config file.
	APIKey string
	// Timeout per call. Default 120s (a local model on CPU is slow).
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

// send builds the body (a copy of the agent's JSON with model/stream swapped)
// and POSTs it. Status >= 300 becomes a *provider.Error here, BEFORE any byte
// reaches the client: that is what allows fallback in streaming too.
func (p *Provider) send(ctx context.Context, c provider.Call, stream bool) (*http.Response, error) {
	body := make(map[string]any, len(c.Raw)+1)
	for k, v := range c.Raw {
		body[k] = v
	}
	body["model"] = c.Model
	body["stream"] = stream
	delete(body, "stream_options")
	if stream {
		// We always ask the upstream for usage (metrics and billing). If the
		// agent asked too, the gateway returns it; otherwise it stays with us.
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if c.Request.EffectiveMaxTokens() == 0 && c.DefaultMaxTokens > 0 {
		body["max_tokens"] = c.DefaultMaxTokens
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, provider.Errorf(http.StatusBadRequest, "invalid_request", "serializing request: %v", err)
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
		return nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "response is not chat JSON: %v", err)
	}
	if len(out.Choices) == 0 {
		return nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "response without choices")
	}
	return &out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s… (%d bytes)", s[:n], len(s))
}
