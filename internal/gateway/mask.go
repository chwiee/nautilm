package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/chwiee/pedagio/internal/auth"
	"github.com/chwiee/pedagio/pkg/openai"
)

// Campos de mensagem que são estrutura, não conteúdo: não mascarar.
var structural = map[string]bool{"role": true, "name": true, "tool_call_id": true, "id": true, "type": true}

// maskChat mascara o texto de TODAS as mensagens (system, user, assistant,
// resultado de tool e argumentos de tool_call) direto no JSON cru, e remonta
// o pedido a partir dele. Por que no cru? O provider openaicompat repassa o
// mapa cru; o Bedrock usa o struct. Mascarando a fonte e remontando, os dois
// recebem exatamente o mesmo texto limpo.
//
// Só mexe em "messages": definição de tools, parâmetros etc. ficam intactos.
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
		return counts, nil // nada mudou: evita remontar à toa
	}
	b, err := json.Marshal(rawMap)
	if err != nil {
		return counts, err
	}
	var masked openai.ChatRequest
	if err := json.Unmarshal(b, &masked); err != nil {
		return counts, fmt.Errorf("remontando pedido mascarado: %w", err)
	}
	*req = masked
	return counts, nil
}

// maskValue anda no JSON e mascara cada string de conteúdo.
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

// maskEmbed mascara os textos de um pedido de embeddings. O vetor de
// "[CPF]" é o mesmo na indexação e na consulta: a busca continua coerente.
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

// reportMasked conta na métrica, loga (só o NOME da regra, nunca o valor)
// e avisa o agente pelo header X-Pedagio-Masked (ex: "cpf=2,email=1").
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
	w.Header().Set("X-Pedagio-Masked", summary)
	g.log.Info("mascarado", "agent", id.Agent, "rules", summary)
}
