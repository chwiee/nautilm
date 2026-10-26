// Package bedrock translates the OpenAI format to the Amazon Bedrock
// Converse API and back.
//
// Credentials: the default AWS chain (Pod Identity, IRSA, local profile).
// ONLY nautilm needs Bedrock permissions; that is the whole point:
//
//	bedrock:InvokeModel                   (Converse, embeddings)
//	bedrock:InvokeModelWithResponseStream (ConverseStream, when the agent streams)
//
// Why Converse and not InvokeModel? One message and tool-calling format for
// every model (Claude, Nova, Llama...). Switching models is switching the ID
// in the nautilm config.
package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

type Config struct {
	Region string // empty = AWS_REGION
}

// Converser is the part of the SDK we use: an interface so it can be tested.
type Converser interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

type Provider struct {
	api    Converser
	stream StreamAPI // nil = no real streaming (the gateway simulates it)
	invoke Invoker   // nil = no embeddings
}

// New loads the default AWS credentials.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	var opts []func(*config.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, config.WithRegion(cfg.Region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bedrock: loading AWS credentials: %w", err)
	}
	client := bedrockruntime.NewFromConfig(awsCfg)
	p := NewWithAPI(client, sdkStream{client})
	p.invoke = client
	return p, nil
}

// NewWithAPI injects the clients (tests). stream may be nil.
func NewWithAPI(api Converser, stream StreamAPI) *Provider {
	return &Provider{api: api, stream: stream}
}

func (p *Provider) Chat(ctx context.Context, c provider.Call) (*openai.ChatResponse, error) {
	in, err := buildInput(c)
	if err != nil {
		return nil, err
	}
	out, err := p.api.Converse(ctx, in)
	if err != nil {
		return nil, mapError(err)
	}
	return parseOutput(c.Model, out)
}

// buildInput does the outbound translation. The differences that matter:
//
//  1. The system prompt is NOT a message: it goes in the System field.
//  2. Only user and assistant exist. A tool result is a toolResult block
//     INSIDE a user message.
//  3. Roles must alternate. Consecutive messages with the same role (e.g.
//     two tool results) are merged into one.
//  4. Tool arguments are a JSON string in OpenAI; a document in Converse.
func buildInput(c provider.Call) (*bedrockruntime.ConverseInput, error) {
	r := c.Request
	if r.ResponseFormat != nil && r.ResponseFormat.Type == "json_schema" {
		return nil, provider.Errorf(http.StatusBadRequest, "unsupported", "response_format json_schema is not supported by the bedrock provider yet")
	}

	in := &bedrockruntime.ConverseInput{ModelId: aws.String(c.Model)}
	inf := &types.InferenceConfiguration{}
	if mt := r.EffectiveMaxTokens(); mt > 0 {
		inf.MaxTokens = aws.Int32(int32(mt))
	} else if c.DefaultMaxTokens > 0 {
		inf.MaxTokens = aws.Int32(int32(c.DefaultMaxTokens))
	}
	if r.Temperature != nil {
		inf.Temperature = aws.Float32(float32(*r.Temperature))
	}
	if r.TopP != nil {
		inf.TopP = aws.Float32(float32(*r.TopP))
	}
	if len(r.Stop) > 0 {
		inf.StopSequences = r.Stop
	}
	in.InferenceConfig = inf

	for i, m := range r.Messages {
		if m.Content.HasNonText {
			return nil, provider.Errorf(http.StatusBadRequest, "unsupported", "message %d: only text content is supported (no images yet)", i)
		}
		var role types.ConversationRole
		var blocks []types.ContentBlock
		switch m.Role {
		case "system", "developer":
			if m.Content.Text != "" {
				in.System = append(in.System, &types.SystemContentBlockMemberText{Value: m.Content.Text})
			}
			continue
		case "user":
			role = types.ConversationRoleUser
			blocks = append(blocks, &types.ContentBlockMemberText{Value: nonEmpty(m.Content.Text)})
		case "assistant":
			role = types.ConversationRoleAssistant
			if m.Content.Text != "" {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Content.Text})
			}
			for _, tc := range m.ToolCalls {
				args, err := argsDoc(tc.Function.Arguments)
				if err != nil {
					return nil, provider.Errorf(http.StatusBadRequest, "invalid_request", "arguments of tool %s are not JSON: %v", tc.Function.Name, err)
				}
				blocks = append(blocks, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
					ToolUseId: aws.String(tc.ID), Name: aws.String(tc.Function.Name), Input: document.NewLazyDocument(args),
				}})
			}
		case "tool":
			role = types.ConversationRoleUser
			blocks = append(blocks, &types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
				ToolUseId: aws.String(m.ToolCallID),
				Content:   []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: nonEmpty(m.Content.Text)}},
			}})
		default:
			return nil, provider.Errorf(http.StatusBadRequest, "invalid_request", "unknown role %q", m.Role)
		}
		if len(blocks) == 0 {
			continue
		}
		if n := len(in.Messages); n > 0 && in.Messages[n-1].Role == role {
			in.Messages[n-1].Content = append(in.Messages[n-1].Content, blocks...)
			continue
		}
		in.Messages = append(in.Messages, types.Message{Role: role, Content: blocks})
	}
	if len(in.Messages) == 0 {
		return nil, provider.Errorf(http.StatusBadRequest, "invalid_request", "no user/assistant message")
	}

	if len(r.Tools) > 0 {
		tc := &types.ToolConfiguration{}
		for _, t := range r.Tools {
			schema, err := schemaDoc(t.Function.Parameters)
			if err != nil {
				return nil, provider.Errorf(http.StatusBadRequest, "invalid_request", "schema of tool %s: %v", t.Function.Name, err)
			}
			tc.Tools = append(tc.Tools, &types.ToolMemberToolSpec{Value: types.ToolSpecification{
				Name:        aws.String(t.Function.Name),
				Description: aws.String(nonEmpty(t.Function.Description)),
				InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)},
			}})
		}
		tc.ToolChoice = toolChoice(r.ToolChoice)
		in.ToolConfig = tc
	}
	return in, nil
}

// toolChoice: "auto" → Auto, "required" → Any, {"function":{"name":X}} → Tool X.
// "none" does not exist in Converse: it stays auto (tools must remain
// declared if the history has toolUse blocks).
func toolChoice(v any) types.ToolChoice {
	switch x := v.(type) {
	case string:
		if x == "required" {
			return &types.ToolChoiceMemberAny{Value: types.AnyToolChoice{}}
		}
	case map[string]any:
		if fn, ok := x["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				return &types.ToolChoiceMemberTool{Value: types.SpecificToolChoice{Name: aws.String(name)}}
			}
		}
	}
	return nil // nil = auto (Converse default)
}

// parseOutput does the inbound translation.
func parseOutput(model string, out *bedrockruntime.ConverseOutput) (*openai.ChatResponse, error) {
	msg, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "unexpected Bedrock output (stopReason=%s)", out.StopReason)
	}
	res := openai.ResponseMessage{Role: "assistant"}
	var text strings.Builder
	for _, b := range msg.Value.Content {
		switch v := b.(type) {
		case *types.ContentBlockMemberText:
			text.WriteString(v.Value)
		case *types.ContentBlockMemberToolUse:
			// The SDK document already knows how to marshal itself to JSON:
			// exactly what OpenAI wants in arguments (a JSON string).
			raw := []byte("{}")
			if v.Value.Input != nil {
				b, err := v.Value.Input.MarshalSmithyDocument()
				if err != nil {
					return nil, provider.Errorf(http.StatusBadGateway, "upstream_invalid", "arguments of %s: %v", aws.ToString(v.Value.Name), err)
				}
				if len(b) > 0 && string(b) != "null" {
					raw = b
				}
			}
			res.ToolCalls = append(res.ToolCalls, openai.ToolCall{
				ID: aws.ToString(v.Value.ToolUseId), Type: "function",
				Function: openai.FunctionCall{Name: aws.ToString(v.Value.Name), Arguments: string(raw)},
			})
		}
	}
	if s := text.String(); s != "" || len(res.ToolCalls) == 0 {
		res.Content = &s
	}

	return &openai.ChatResponse{
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openai.Choice{{Message: res, FinishReason: finishReason(out.StopReason)}},
		Usage:   toUsage(out.Usage),
	}, nil
}

// toUsage maps Bedrock token usage to the normalized nautilm usage.
// Bedrock reports inputTokens WITHOUT the cache reads/writes; OpenAI counts
// them inside prompt_tokens. We follow OpenAI so the price math is the same
// for every provider (see openai.Usage).
func toUsage(u *types.TokenUsage) *openai.Usage {
	if u == nil {
		return nil
	}
	read, write := int(aws.ToInt32(u.CacheReadInputTokens)), int(aws.ToInt32(u.CacheWriteInputTokens))
	in := int(aws.ToInt32(u.InputTokens)) + read + write
	out := int(aws.ToInt32(u.OutputTokens))
	usage := &openai.Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out}
	if read > 0 || write > 0 {
		usage.PromptTokensDetails = &openai.PromptTokensDetails{CachedTokens: read, CacheWriteTokens: write}
	}
	return usage
}

func finishReason(r types.StopReason) string {
	switch r {
	case types.StopReasonToolUse:
		return "tool_calls"
	case types.StopReasonMaxTokens:
		return "length"
	case types.StopReasonGuardrailIntervened, types.StopReasonContentFiltered:
		return "content_filter"
	default:
		return "stop"
	}
}

// mapError turns AWS errors into HTTP statuses so the gateway can decide
// between returning to the agent or trying the fallback.
func mapError(err error) error {
	var api smithy.APIError
	if !errors.As(err, &api) {
		if errors.Is(err, context.DeadlineExceeded) {
			return provider.Errorf(http.StatusGatewayTimeout, "timeout", "Bedrock did not answer in time")
		}
		return provider.Errorf(http.StatusBadGateway, "upstream_error", "%v", err)
	}
	status := http.StatusBadGateway
	switch api.ErrorCode() {
	case "ValidationException":
		status = http.StatusBadRequest
	case "ThrottlingException", "ServiceQuotaExceededException":
		status = http.StatusTooManyRequests
	case "ModelTimeoutException":
		status = http.StatusGatewayTimeout
	case "ServiceUnavailableException", "ModelNotReadyException", "InternalServerException":
		status = http.StatusServiceUnavailable
	case "AccessDeniedException", "ResourceNotFoundException":
		// A NAUTILM configuration error (IAM or model not enabled), not the
		// agent's: 502 and try the fallback.
		status = http.StatusBadGateway
	}
	return provider.Errorf(status, api.ErrorCode(), "%s", api.ErrorMessage())
}

// argsDoc turns the JSON arguments string into a generic value. The SDK
// document needs map/slice, not bytes.
func argsDoc(s string) (any, error) {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}, nil
	}
	var v any
	err := json.Unmarshal([]byte(s), &v)
	return v, err
}

func schemaDoc(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	var v any
	err := json.Unmarshal(raw, &v)
	return v, err
}

// Converse rejects empty text blocks.
func nonEmpty(s string) string {
	if s == "" {
		return " "
	}
	return s
}

// WithInvoker injects the InvokeModel client (embeddings): tests.
func (p *Provider) WithInvoker(i Invoker) *Provider { p.invoke = i; return p }
