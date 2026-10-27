package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// capture keeps the last Call the gateway made.
type capture struct{ last provider.Call }

func (c *capture) Chat(_ context.Context, call provider.Call) (*openai.ChatResponse, error) {
	c.last = call
	s := "ok"
	return &openai.ChatResponse{Object: "chat.completion", Choices: []openai.Choice{{Message: openai.ResponseMessage{Role: "assistant", Content: &s}}},
		Usage: &openai.Usage{PromptTokens: 10, CompletionTokens: 1}}, nil
}

// The cache marker survives masking, and masking still reaches the text of
// the marked part (the part list is rebuilt from the masked JSON).
func TestCacheMarkerSurvivesMasking(t *testing.T) {
	p := &capture{}
	yaml := strings.Replace(testConfig, "smart: {provider: a, model: big-model, fallback: [fast]}",
		"smart: {provider: a, model: big-model, fallback: [fast], prompt_cache: explicit}", 1)
	e := setupCfg(t, yaml, map[string]provider.Provider{"a": p, "b": &fake{}})
	resp := post(t, e.srv, "tok-k8s", `{"model":"smart","messages":[
	  {"role":"system","content":[{"type":"text","text":"guides; owner ana@corp.example","cache_control":{"type":"ephemeral"}}]},
	  {"role":"user","content":"hi"}]}`)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if p.last.PromptCache != "explicit" {
		t.Errorf("alias prompt_cache must reach the provider: %q", p.last.PromptCache)
	}
	parts := p.last.Request.Messages[0].Content.Parts
	if len(parts) != 1 || !parts[0].Cache {
		t.Fatalf("marker lost: %+v", parts)
	}
	if strings.Contains(parts[0].Text, "ana@corp.example") || !strings.Contains(parts[0].Text, "[EMAIL]") {
		t.Errorf("the marked part must be masked too: %q", parts[0].Text)
	}
	// The raw JSON (forwarded to OpenAI-compatible providers) keeps the marker.
	sys := p.last.Raw["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if sys["cache_control"] == nil || strings.Contains(sys["text"].(string), "ana@") {
		t.Errorf("raw: %v", sys)
	}
}
