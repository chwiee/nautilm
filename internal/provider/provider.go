// Package provider define o contrato que todo backend de LLM cumpre
// (Bedrock, Ollama, vLLM, OpenAI...). O gateway só conhece esta interface:
// adicionar um provedor novo = implementar Chat e registrar um "type" no main.
package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/chwiee/pedagio/pkg/openai"
)

// Call é uma chamada já resolvida pelo gateway.
type Call struct {
	// Model é o ID real no provedor (o apelido já foi traduzido).
	Model string
	// Request é o pedido do agente, já validado. Stream vem sempre false:
	// o gateway cuida do streaming para o cliente.
	Request *openai.ChatRequest
	// Raw é o JSON original do agente. Providers compatíveis com OpenAI
	// repassam ele (só trocando model/stream) para não perder campos extras.
	Raw map[string]any
	// MaxTokens padrão do apelido, usado quando o agente não informa.
	DefaultMaxTokens int
	// Header traz cabeçalhos que vale propagar (ex: traceparent).
	Header http.Header
}

// Provider é um backend de LLM.
type Provider interface {
	Chat(ctx context.Context, c Call) (*openai.ChatResponse, error)
}

// Error é um erro com status HTTP. O gateway usa Status para decidir:
//   - devolver ao agente (4xx: o pedido está errado, tentar outro modelo não ajuda);
//   - tentar o fallback (429 e 5xx: problema do provedor).
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

// Retryable diz se vale tentar o próximo modelo da lista de fallback.
func (e *Error) Retryable() bool { return e.Status == http.StatusTooManyRequests || e.Status >= 500 }

// Errorf cria um *Error.
func Errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}
