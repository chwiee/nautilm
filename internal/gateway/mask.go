package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/chwiee/nautilm/internal/auth"
	"github.com/chwiee/nautilm/pkg/openai"
)

// Message fields that are structure, not content: never masked.
var structural = map[string]bool{"role": true, "name": true, "tool_call_id": true, "id": true, "type": true}

// maskChat masks the text of ALL messages (system, user, assistant, tool
// results and tool_call arguments) directly in the raw JSON, and rebuilds
// the request from it. Why the raw JSON? The openaicompat provider forwards
// the raw map; Bedrock uses the struct. Masking the source and rebuilding
// means both get exactly the same clean text.
//
// Only "messages" is touched: tool definitions, parameters etc. stay intact.
func (g *Gateway) maskChat(rawMap map[string]any, req *openai.ChatRequest) (map[string]int, error) {
	counts := map[string]int{}
	if g.masker == nil || rawMap == nil {
		return counts, nil
	}
	msgs, ok := rawMap["messages"]
	if !ok {
		return counts, nil
	}
	rawMap["messages"] = g.maskValue(msgs, "", counts)
	if len(counts) == 0 {
		return counts, nil // nothing changed: no need to rebuild
	}
	b, err := json.Marshal(rawMap)
	if err != nil {
		return counts, err
	}
	var masked openai.ChatRequest
	if err := json.Unmarshal(b, &masked); err != nil {
		return counts, fmt.Errorf("rebuilding the masked request: %w", err)
	}
	*req = masked
	return counts, nil
}

// maskValue walks the JSON and masks every content string.
func (g *Gateway) maskValue(v any, key string, counts map[string]int) any {
	switch t := v.(type) {
	case string:
		if structural[key] {
			return t
		}
		return g.masker.Apply(t, counts)
	case []any:
		for i := range t {
			t[i] = g.maskValue(t[i], key, counts)
		}
		return t
	case map[string]any:
		for k, x := range t {
			t[k] = g.maskValue(x, k, counts)
		}
		return t
	}
	return v
}

// maskEmbed masks the texts of an embeddings request. The vector of
// "[CPF]" is the same at indexing and at query time: search stays coherent.
func (g *Gateway) maskEmbed(input []string) map[string]int {
	counts := map[string]int{}
	if g.masker == nil {
		return counts
	}
	for i, s := range input {
		input[i] = g.masker.Apply(s, counts)
	}
	return counts
}

// reportMasked counts in the metric, logs (the rule NAME only, never the
// value) and tells the agent through X-Nautilm-Masked (e.g. "cpf=2,email=1").
func (g *Gateway) reportMasked(w http.ResponseWriter, id auth.Identity, counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	var parts []string
	for rule, n := range counts {
		g.metrics.Masked(id.Agent, rule, n)
		parts = append(parts, fmt.Sprintf("%s=%d", rule, n))
	}
	sort.Strings(parts)
	summary := strings.Join(parts, ",")
	w.Header().Set("X-Nautilm-Masked", summary)
	g.log.Info("masked", "agent", id.Agent, "rules", summary)
}
