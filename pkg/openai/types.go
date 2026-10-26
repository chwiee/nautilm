// Package openai holds the OpenAI chat API types (/v1/chat/completions):
// the "language" nautilm speaks with agents.
//
// Why OpenAI and not our own format? Practically every SDK, framework and
// tool already speaks it. A new agent only needs to point its base URL to
// nautilm; it does not need to import anything of ours.
//
// The package lives in pkg/ on purpose: Go agents that want ready-made types
// can import it.
package openai

import (
	"encoding/json"
	"strings"
)

// ChatRequest is the body of POST /v1/chat/completions.
// Only the fields nautilm understands are here; the "openai" provider
// forwards the original JSON, so extra fields are not lost there.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  any       `json:"tool_choice,omitempty"` // "auto" | "none" | "required" | {"type":"function","function":{"name":"x"}}
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	// MaxCompletionTokens is the new name of max_tokens; it wins if both are sent.
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stop                StringOrList    `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	ResponseFormat      *ResponseFormat `json:"response_format,omitempty"`
	User                string          `json:"user,omitempty"`
}

// EffectiveMaxTokens returns the requested limit (0 = not set).
func (r *ChatRequest) EffectiveMaxTokens() int {
	if r.MaxCompletionTokens != nil {
		return *r.MaxCompletionTokens
	}
	if r.MaxTokens != nil {
		return *r.MaxTokens
	}
	return 0
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ResponseFormat struct {
	Type string `json:"type"` // "text" | "json_object" | "json_schema"
}

// Message is one conversation message.
type Message struct {
	Role       string     `json:"role"` // system | developer | user | assistant | tool
	Content    Content    `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Content accepts both OpenAI shapes: a plain string or a list of parts
// [{"type":"text","text":"..."}]. Only text is kept (images are not
// supported by nautilm yet).
type Content struct {
	Text string
	// HasNonText tells that non-text parts were sent (e.g. image_url).
	HasNonText bool
}

func (c *Content) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &c.Text)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return err
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		} else {
			c.HasNonText = true
		}
	}
	c.Text = sb.String()
	return nil
}

func (c Content) MarshalJSON() ([]byte, error) { return json.Marshal(c.Text) }

// Text builds a text Content.
func Text(s string) Content { return Content{Text: s} }

// ToolCall is the model asking to run a function.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"` // streaming only
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"` // always "function"
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name string `json:"name,omitempty"`
	// Arguments is JSON INSIDE a string: that is how OpenAI defines it.
	Arguments string `json:"arguments"`
}

// Tool describes a function the model may call.
type Tool struct {
	Type     string      `json:"type"` // "function"
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
}

// ChatResponse is the non-streaming response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"` // stop | length | tool_calls | content_filter
}

// ResponseMessage uses *string for Content because OpenAI sends null when
// the model only asked for tools.
type ResponseMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Usage counts tokens: this is what becomes the bill.
//
// Normalized meaning inside nautilm (every provider maps to it):
//   - PromptTokens: ALL input tokens, including cache reads and cache writes
//     (OpenAI semantics);
//   - PromptTokensDetails.CachedTokens: input tokens read from the cache;
//   - PromptTokensDetails.CacheWriteTokens: input tokens written to the cache
//     (nautilm extension; Bedrock/Anthropic charge them differently);
//   - Cost: cost in USD reported BY THE PROVIDER, when it reports one
//     (e.g. OpenRouter). When present, it wins over the price table.
type Usage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	Cost                *float64             `json:"cost,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// CacheRead returns the tokens read from the cache (0 when unknown).
func (u *Usage) CacheRead() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

// CacheWrite returns the tokens written to the cache (0 when unknown).
func (u *Usage) CacheWrite() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CacheWriteTokens
}

// ChatChunk is a streaming event (SSE "data: {...}").
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"` // "chat.completion.chunk"
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ErrorResponse is the OpenAI error format: clients know how to show it.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// ModelList is the response of GET /v1/models.
type ModelList struct {
	Object string  `json:"object"` // "list"
	Data   []Model `json:"data"`
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // "model"
	OwnedBy string `json:"owned_by"`
}

// StringOrList accepts "stop": "x" or "stop": ["x","y"].
type StringOrList []string

func (s *StringOrList) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// EmbeddingRequest is the body of POST /v1/embeddings.
type EmbeddingRequest struct {
	Model string       `json:"model"`
	Input StringOrList `json:"input"` // one text or a list
	// Dimensions asks for smaller vectors (Titan v2: 256, 512 or 1024).
	Dimensions *int   `json:"dimensions,omitempty"`
	User       string `json:"user,omitempty"`
}

// EmbeddingResponse is the response of POST /v1/embeddings.
type EmbeddingResponse struct {
	Object string          `json:"object"` // "list"
	Data   []EmbeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  *Usage          `json:"usage,omitempty"`
}

type EmbeddingData struct {
	Object    string    `json:"object"` // "embedding"
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}
