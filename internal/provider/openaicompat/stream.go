package openaicompat

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// ChatStream forwards the upstream SSE chunk by chunk.
//
// Each chunk is read as a generic map (not a struct) so fields nautilm does
// not know are kept, e.g. "reasoning_content" from thinking models. We only
// swap id and model (the agent sees the alias) and strip usage, which the
// gateway returns at the end if the agent asked for it.
func (p *Provider) ChatStream(ctx context.Context, c provider.Call, emit provider.Emit) (*openai.Usage, error) {
	resp, err := p.send(ctx, c, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var usage *openai.Usage
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20) // a chunk with tool arguments can be large

	for sc.Scan() {
		line := sc.Text()
		// SSE: only "data:" lines matter. Comments (":") and empty lines
		// (event separators) are ignored.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return usage, nil
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return usage, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "invalid SSE chunk: %v", err)
		}
		if e, ok := chunk["error"]; ok {
			b, _ := json.Marshal(e)
			return usage, provider.Errorf(http.StatusBadGateway, "upstream_error", "error in the middle of the stream: %s", truncate(string(b), 500))
		}
		if u, ok := chunk["usage"]; ok && u != nil {
			var parsed openai.Usage
			b, _ := json.Marshal(u)
			if json.Unmarshal(b, &parsed) == nil {
				usage = &parsed
			}
			delete(chunk, "usage")
		}
		// Usage-only chunk (empty choices): already kept, do not forward.
		if choices, _ := chunk["choices"].([]any); len(choices) == 0 {
			continue
		}
		chunk["id"] = c.ID
		chunk["model"] = c.Alias
		if err := emit(chunk); err != nil {
			return usage, err
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return usage, ctx.Err()
		}
		return usage, provider.Errorf(http.StatusBadGateway, "upstream_read", "%v", err)
	}
	// Upstream closed without [DONE]: some servers do that; treat it as a normal end.
	return usage, nil
}
