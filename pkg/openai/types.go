// Package openai tem os tipos do formato de chat da API OpenAI
// (/v1/chat/completions), que é o "idioma" que o pedagio fala com os agentes.
//
// Por que OpenAI e não um formato nosso? Porque praticamente todo SDK,
// framework e ferramenta já fala esse formato. Um agente novo só precisa
// apontar a base URL para o pedagio — não precisa importar nada nosso.
//
// O pacote fica em pkg/ de propósito: agentes Go que quiserem tipos prontos
// podem importar daqui.
package openai

import (
	"encoding/json"
	"strings"
)

// ChatRequest é o corpo de POST /v1/chat/completions.
// Só os campos que o pedagio entende estão aqui; o provider "openai"
// repassa o JSON original, então campos extras não se perdem nele.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  any       `json:"tool_choice,omitempty"` // "auto" | "none" | "required" | {"type":"function","function":{"name":"x"}}
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	// MaxCompletionTokens é o nome novo de max_tokens; vence se os dois vierem.
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stop                StringOrList    `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	ResponseFormat      *ResponseFormat `json:"response_format,omitempty"`
	User                string          `json:"user,omitempty"`
}

// EffectiveMaxTokens devolve o limite pedido (0 = não informado).
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

// Message é uma mensagem da conversa.
type Message struct {
	Role       string     `json:"role"` // system | developer | user | assistant | tool
	Content    Content    `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Content aceita os dois formatos da OpenAI: string simples ou lista de
// partes [{"type":"text","text":"..."}]. Guardamos só o texto (imagens
// ainda não são suportadas pelo pedagio).
type Content struct {
	Text string
	// HasNonText indica que vieram partes que não são texto (ex: image_url).
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

// Text cria um Content com texto.
func Text(s string) Content { return Content{Text: s} }

// ToolCall é o pedido do modelo para executar uma função.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"` // só em streaming
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"` // sempre "function"
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name string `json:"name,omitempty"`
	// Arguments é JSON DENTRO de uma string — é assim que a OpenAI define.
	Arguments string `json:"arguments"`
}

// Tool descreve uma função que o modelo pode pedir.
type Tool struct {
	Type     string      `json:"type"` // "function"
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
}

// ChatResponse é a resposta sem streaming.
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

// ResponseMessage usa *string em Content porque a OpenAI manda null quando
// o modelo só pediu tools.
type ResponseMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Usage conta tokens — é o que vira custo na fatura.
type Usage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// ChatChunk é um evento de streaming (SSE "data: {...}").
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

// ErrorResponse é o formato de erro da OpenAI — clientes sabem exibir.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// ModelList é a resposta de GET /v1/models.
type ModelList struct {
	Object string  `json:"object"` // "list"
	Data   []Model `json:"data"`
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // "model"
	OwnedBy string `json:"owned_by"`
}

// StringOrList aceita "stop": "x" ou "stop": ["x","y"].
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

// EmbeddingRequest é o corpo de POST /v1/embeddings.
type EmbeddingRequest struct {
	Model string       `json:"model"`
	Input StringOrList `json:"input"` // um texto ou uma lista
	// Dimensions pede vetores menores (Titan v2: 256, 512 ou 1024).
	Dimensions *int   `json:"dimensions,omitempty"`
	User       string `json:"user,omitempty"`
}

// EmbeddingResponse é a resposta de POST /v1/embeddings.
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
