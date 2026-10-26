package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// Embed forwards to POST /embeddings (Ollama /v1, vLLM, OpenAI...).
func (p *Provider) Embed(ctx context.Context, c provider.EmbedCall) ([][]float32, *openai.Usage, error) {
	body := map[string]any{"model": c.Model, "input": c.Input}
	if c.Dimensions > 0 {
		body["dimensions"] = c.Dimensions
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/embeddings", bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	if tp := c.Header.Get("traceparent"); tp != "" {
		req.Header.Set("traceparent", tp)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, nil, provider.Errorf(http.StatusBadGateway, "upstream_unreachable", "%v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, nil, provider.Errorf(http.StatusBadGateway, "upstream_read", "%v", err)
	}
	if resp.StatusCode >= 300 {
		return nil, nil, provider.Errorf(resp.StatusCode, "upstream_error", "%s", truncate(string(raw), 500))
	}
	var out openai.EmbeddingResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "response is not embeddings JSON: %v", err)
	}
	if len(out.Data) != len(c.Input) {
		return nil, nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "asked for %d embeddings, got %d", len(c.Input), len(out.Data))
	}
	// The spec says "index" defines the order; we do not trust the array order.
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		vecs[i] = d.Embedding
	}
	return vecs, out.Usage, nil
}
