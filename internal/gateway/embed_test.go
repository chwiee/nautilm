package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/chwiee/nautilm/internal/config"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

type fakeEmbedder struct {
	fake
	err   error
	dims  int
	input []string
}

func (f *fakeEmbedder) Embed(_ context.Context, c provider.EmbedCall) ([][]float32, *openai.Usage, error) {
	f.calls = append(f.calls, c.Model)
	if f.err != nil {
		return nil, nil, f.err
	}
	f.dims = c.Dimensions
	f.input = append([]string(nil), c.Input...)
	out := make([][]float32, len(c.Input))
	for i := range c.Input {
		out[i] = []float32{float32(i), 1}
	}
	return out, &openai.Usage{PromptTokens: 3 * len(c.Input), TotalTokens: 3 * len(c.Input)}, nil
}

func postEmbed(t *testing.T, env testEnv, token, body string) *http.Response {
	t.Helper()
	return do(t, http.MethodPost, env.srv.URL+"/v1/embeddings", token, body)
}

func TestEmbeddings(t *testing.T) {
	e := &fakeEmbedder{}
	env := setup(t, e, &fake{})
	resp := postEmbed(t, env, "tok-k8s", `{"model":"embed","input":["a","b"]}`)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var out openai.EmbeddingResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Data) != 2 || out.Data[1].Index != 1 || out.Model != "embed" || out.Usage.PromptTokens != 6 {
		t.Errorf("response: %+v", out)
	}
	if e.calls[0] != "embed-model" || e.dims != 512 {
		t.Errorf("call: %v dims=%d", e.calls, e.dims)
	}
	// 6 tokens × 0.02 per 1M
	if resp.Header.Get("X-Nautilm-Cost-USD") != "0.00000012" || resp.Header.Get("X-Nautilm-Price-Source") != "table" {
		t.Errorf("cost headers: %v", resp.Header)
	}
	// a single string input also works
	if resp := postEmbed(t, env, "tok-k8s", `{"model":"embed","input":"just one"}`); resp.StatusCode != 200 {
		t.Errorf("string input: %d", resp.StatusCode)
	}
}

func TestEmbeddingsWrongType(t *testing.T) {
	env := setup(t, &fakeEmbedder{}, &fake{})
	if r := postEmbed(t, env, "tok-k8s", `{"model":"smart","input":["a"]}`); r.StatusCode != 400 {
		t.Errorf("chat on /v1/embeddings: %d", r.StatusCode)
	}
	if r := post(t, env.srv, "tok-k8s", fmt.Sprintf(hello, "embed")); r.StatusCode != 400 {
		t.Errorf("embedding on /v1/chat/completions: %d", r.StatusCode)
	}
}

func TestEmbeddingWithoutFallbackInConfig(t *testing.T) {
	_, err := config.Parse([]byte(`
providers: {a: {type: openai, base_url: http://x}}
models:
  e1: {type: embedding, provider: a, model: m1, fallback: [e2]}
  e2: {type: embedding, provider: a, model: m2}
`))
	if err == nil {
		t.Fatal("an embedding with a fallback must be refused by the config")
	}
}
