package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

const upstreamSSE = `: keep-alive comment

data: {"id":"up-1","model":"qwen","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"hmm"}}]}

data: {"id":"up-1","model":"qwen","choices":[{"index":0,"delta":{"content":"hi"}}]}

data: {"id":"up-1","model":"qwen","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"up-1","model":"qwen","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}

data: [DONE]

`

func TestStreamPassThrough(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, upstreamSSE)
	}))
	defer srv.Close()

	var chunks []map[string]any
	usage, err := New(Config{BaseURL: srv.URL}).ChatStream(context.Background(),
		provider.Call{Model: "qwen", ID: "chatcmpl-gw", Alias: "local", Raw: map[string]any{"model": "local"}, Request: &openai.ChatRequest{}},
		func(v any) error { chunks = append(chunks, v.(map[string]any)); return nil })
	if err != nil {
		t.Fatal(err)
	}

	if sent["stream"] != true {
		t.Errorf("the upstream must receive stream=true: %v", sent)
	}
	if so, _ := sent["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("nautilm always asks the upstream for usage: %v", sent["stream_options"])
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks (the usage one stays with the gateway), got %d", len(chunks))
	}
	for _, c := range chunks {
		if c["id"] != "chatcmpl-gw" || c["model"] != "local" {
			t.Errorf("id/model not swapped: %v", c)
		}
	}
	delta := chunks[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["reasoning_content"] != "hmm" {
		t.Errorf("an unknown field must pass intact: %v", delta)
	}
	if usage == nil || usage.TotalTokens != 6 {
		t.Errorf("usage: %+v", usage)
	}
}

func TestStreamErrorWhenOpening(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "limite", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	called := false
	_, err := New(Config{BaseURL: srv.URL}).ChatStream(context.Background(),
		provider.Call{Raw: map[string]any{}, Request: &openai.ChatRequest{}},
		func(any) error { called = true; return nil })
	if pe, ok := err.(*provider.Error); !ok || pe.Status != 429 || called {
		t.Errorf("err=%v emit called=%v", err, called)
	}
}
