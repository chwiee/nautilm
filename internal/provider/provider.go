// Package provider defines the contract every LLM backend fulfils
// (Bedrock, Ollama, vLLM, OpenAI...). The gateway only knows this interface:
// adding a new provider = implementing Chat and registering a "type" in main.
package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/chwiee/nautilm/pkg/openai"
)

// Call is a call already resolved by the gateway.
type Call struct {
	// Model is the real model ID at the provider (the alias was translated).
	Model string
	// Request is the agent's request, already validated. Stream is always
	// false: the gateway handles streaming to the client.
	Request *openai.ChatRequest
	// Raw is the agent's original JSON. OpenAI-compatible providers forward
	// it (only swapping model/stream) so extra fields are not lost.
	Raw map[string]any
	// DefaultMaxTokens of the alias, used when the agent does not send one.
	DefaultMaxTokens int
	// Header carries headers worth propagating (e.g. traceparent).
	Header http.Header
	// ID and Alias go in every streaming chunk: the agent sees a stable id
	// and the alias it asked for, never the real model ID.
	ID    string
	Alias string
}

// Provider is an LLM backend.
type Provider interface {
	Chat(ctx context.Context, c Call) (*openai.ChatResponse, error)
}

// Emit delivers a chunk to the client. The value is serialized to JSON and
// sent as an SSE "data: ..." event. An error means the client gave up: stop
// reading the upstream.
type Emit func(chunk any) error

// Streamer is optional: a provider that streams for real. Providers that
// do not implement it fall back to the gateway's "simulated stream".
//
// Contract rules:
//   - do not emit the usage chunk (the gateway emits it if the client asked);
//   - return the final usage for metrics and billing;
//   - an error BEFORE the first Emit still allows fallback; after it, no.
type Streamer interface {
	ChatStream(ctx context.Context, c Call, emit Emit) (*openai.Usage, error)
}

// EmbedCall is an embeddings request already resolved.
type EmbedCall struct {
	Model      string   // real model ID at the provider
	Input      []string // texts, in order
	Dimensions int      // 0 = model default
	Header     http.Header
}

// Embedder is optional: a provider that can generate embeddings.
// It returns one vector per text, IN THE SAME ORDER as Input, plus usage.
type Embedder interface {
	Embed(ctx context.Context, c EmbedCall) ([][]float32, *openai.Usage, error)
}

// Error is an error with an HTTP status. The gateway uses Status to decide:
//   - return it to the agent (4xx: the request is wrong, another model will not help);
//   - try the fallback (429 and 5xx: provider trouble).
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

// Retryable tells whether trying the next model in the fallback chain makes sense.
func (e *Error) Retryable() bool { return e.Status == http.StatusTooManyRequests || e.Status >= 500 }

// Errorf builds an *Error.
func Errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}
