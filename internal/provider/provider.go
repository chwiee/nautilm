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
	// ID e Alias vão em todo chunk de streaming: o agente vê um id estável
	// e o apelido que pediu, nunca o ID real do modelo.
	ID    string
	Alias string
}

// Provider é um backend de LLM.
type Provider interface {
	Chat(ctx context.Context, c Call) (*openai.ChatResponse, error)
}

// Emit entrega um chunk ao cliente. O valor é serializado em JSON e vai num
// evento SSE "data: ...". Erro = o cliente desistiu: pare de ler o upstream.
type Emit func(chunk any) error

// Streamer é opcional: provider que sabe fazer streaming de verdade.
// Quem não implementa cai no "stream simulado" do gateway.
//
// Regras do contrato:
//   - não emitir o chunk de usage (o gateway emite, se o cliente pediu);
//   - devolver o usage final para as métricas;
//   - erro ANTES do primeiro Emit ainda permite fallback; depois, não.
type Streamer interface {
	ChatStream(ctx context.Context, c Call, emit Emit) (*openai.Usage, error)
}

// EmbedCall é um pedido de embeddings já resolvido.
type EmbedCall struct {
	Model      string   // ID real no provedor
	Input      []string // textos, na ordem
	Dimensions int      // 0 = padrão do modelo
	Header     http.Header
}

// Embedder é opcional: provider que sabe gerar embeddings.
// Devolve um vetor por texto, NA MESMA ORDEM do Input, e o usage.
type Embedder interface {
	Embed(ctx context.Context, c EmbedCall) ([][]float32, *openai.Usage, error)
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
