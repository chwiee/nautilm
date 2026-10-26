package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

func TestPassThroughSwapsModelAndDisablesStream(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	raw := map[string]any{"model": "fast", "stream": true, "seed": 42.0, "messages": []any{}}
	_, err := New(Config{BaseURL: srv.URL}).Chat(context.Background(), provider.Call{
		Model: "qwen2.5:7b", Raw: raw, Request: &openai.ChatRequest{}, DefaultMaxTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "qwen2.5:7b" || got["stream"] != false || got["max_tokens"] != 100.0 {
		t.Errorf("body sent: %v", got)
	}
	if got["seed"] != 42.0 {
		t.Errorf("the extra field (seed) must be forwarded: %v", got)
	}
	if raw["model"] != "fast" {
		t.Errorf("must not change the original map (the fallback reuses it)")
	}
}

func TestUpstreamErrorKeepsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sobrecarregado", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := New(Config{BaseURL: srv.URL}).Chat(context.Background(), provider.Call{Raw: map[string]any{}, Request: &openai.ChatRequest{}})
	if pe, ok := err.(*provider.Error); !ok || pe.Status != 503 || !pe.Retryable() {
		t.Errorf("err = %v", err)
	}
}
