package openaicompat

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

// ChatStream repassa o SSE do upstream chunk a chunk.
//
// Cada chunk é lido como mapa genérico (não struct) para não perder campos
// que o pedagio não conhece — ex: "reasoning_content" de modelos que pensam.
// Só trocamos id e model (o agente vê o apelido) e tiramos o usage, que o
// gateway devolve no fim se o agente pediu.
func (p *Provider) ChatStream(ctx context.Context, c provider.Call, emit provider.Emit) (*openai.Usage, error) {
	resp, err := p.send(ctx, c, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var usage *openai.Usage
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20) // um chunk com argumentos de tool pode ser grande

	for sc.Scan() {
		line := sc.Text()
		// SSE: só interessam linhas "data:". Comentários (":") e linhas
		// vazias (separador de evento) são ignorados.
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
			return usage, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "chunk SSE inválido: %v", err)
		}
		if e, ok := chunk["error"]; ok {
			b, _ := json.Marshal(e)
			return usage, provider.Errorf(http.StatusBadGateway, "upstream_error", "erro no meio do stream: %s", truncate(string(b), 500))
		}
		if u, ok := chunk["usage"]; ok && u != nil {
			var parsed openai.Usage
			b, _ := json.Marshal(u)
			if json.Unmarshal(b, &parsed) == nil {
				usage = &parsed
			}
			delete(chunk, "usage")
		}
		// Chunk só de usage (choices vazio): já guardamos, não repassa.
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
	// Upstream fechou sem [DONE]: alguns servidores fazem isso; tratamos como fim normal.
	return usage, nil
}
