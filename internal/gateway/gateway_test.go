package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

func TestChatResolvesAlias(t *testing.T) {
	a, b := &fake{}, &fake{}
	env := setup(t, a, b)
	resp := post(t, env.srv, "tok-k8s", fmt.Sprintf(hello, "smart"))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out openai.ChatResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Model != "smart" {
		t.Errorf("the agent must see the alias, saw %q", out.Model)
	}
	if resp.Header.Get("X-Nautilm-Model") != "big-model" || resp.Header.Get("X-Nautilm-Provider") != "a" {
		t.Errorf("served headers: %v", resp.Header)
	}
	if len(a.calls) != 1 || a.calls[0] != "big-model" {
		t.Errorf("provider a called with %v", a.calls)
	}
	if !strings.Contains(metricsText(env.metrics), `nautilm_tokens_total{agent="k8s",alias="smart",provider="a",model="big-model",kind="input"} 10`) {
		t.Errorf("token metric missing:\n%s", metricsText(env.metrics))
	}
}

func TestFallbackOnlyOnRetryableErrors(t *testing.T) {
	// 503 on "smart" → tries "fast".
	a, b := &fake{err: provider.Errorf(503, "x", "down")}, &fake{}
	env := setup(t, a, b)
	resp := post(t, env.srv, "tok-k8s", fmt.Sprintf(hello, "smart"))
	if resp.StatusCode != 200 || resp.Header.Get("X-Nautilm-Alias") != "fast" {
		t.Fatalf("expected fallback to fast: status %d alias %q", resp.StatusCode, resp.Header.Get("X-Nautilm-Alias"))
	}

	// 400 on "smart" → does NOT try "fast" (the request is wrong).
	a2, b2 := &fake{err: provider.Errorf(400, "ValidationException", "bad")}, &fake{}
	env2 := setup(t, a2, b2)
	resp = post(t, env2.srv, "tok-k8s", fmt.Sprintf(hello, "smart"))
	if resp.StatusCode != 400 || len(b2.calls) != 0 {
		t.Fatalf("a 400 must not fall back: status %d, fast calls %v", resp.StatusCode, b2.calls)
	}
}

func TestPermissions(t *testing.T) {
	env := setup(t, &fake{}, &fake{})
	cases := []struct {
		name, token, model string
		want               int
	}{
		{"no token", "", "fast", 401},
		{"wrong token", "nope", "fast", 401},
		{"wildcard may use fast", "tok-cli", "fast", 200},
		{"wildcard may not use smart", "tok-cli", "smart", 403},
		{"raw model ID is refused", "tok-k8s", "big-model", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := post(t, env.srv, c.token, fmt.Sprintf(hello, c.model))
			if resp.StatusCode != c.want {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status %d, want %d: %s", resp.StatusCode, c.want, b)
			}
		})
	}
}

func TestSimulatedStream(t *testing.T) {
	env := setup(t, &fake{}, &fake{})
	resp := post(t, env.srv, "tok-cli", `{"model":"fast","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	var content string
	var sawUsage, sawDone bool
	for _, line := range readSSE(t, resp.Body) {
		if line == "[DONE]" {
			sawDone = true
			continue
		}
		var c openai.ChatChunk
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("invalid chunk %q: %v", line, err)
		}
		for _, ch := range c.Choices {
			content += ch.Delta.Content
		}
		if c.Usage != nil {
			sawUsage = true
		}
	}
	if content != "ok from small-model" || !sawUsage || !sawDone {
		t.Errorf("content=%q usage=%v done=%v", content, sawUsage, sawDone)
	}
}

func TestModelsListsOnlyAllowed(t *testing.T) {
	env := setup(t, &fake{}, &fake{})
	resp := do(t, http.MethodGet, env.srv.URL+"/v1/models", "tok-cli", "")
	var list openai.ModelList
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Data) != 1 || list.Data[0].ID != "fast" || list.Data[0].OwnedBy != "nautilm" {
		t.Errorf("cli should only see fast: %+v", list.Data)
	}
}
