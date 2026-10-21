package bedrock

import (
	"context"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

// EventReader é o que o SDK devolve em ConverseStreamOutput.GetStream().
// Interface própria porque o campo do SDK é privado: assim dá para testar.
type EventReader interface {
	Events() <-chan types.ConverseStreamOutput
	Close() error
	Err() error
}

// StreamAPI abre o ConverseStream.
type StreamAPI interface {
	ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput) (EventReader, error)
}

// sdkStream adapta o cliente do SDK à StreamAPI.
type sdkStream struct{ c *bedrockruntime.Client }

func (s sdkStream) ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput) (EventReader, error) {
	out, err := s.c.ConverseStream(ctx, in)
	if err != nil {
		return nil, err
	}
	return out.GetStream(), nil
}

// ChatStream traduz os eventos do ConverseStream em chunks OpenAI:
//
//	messageStart                → delta {role: assistant}
//	contentBlockDelta (texto)   → delta {content}
//	contentBlockStart (toolUse) → delta {tool_calls: [{index, id, name, arguments: ""}]}
//	contentBlockDelta (toolUse) → delta {tool_calls: [{index, arguments: "<pedaço do JSON>"}]}
//	messageStop                 → finish_reason
//	metadata                    → usage (devolvido ao gateway)
//
// Detalhe: o Bedrock numera os blocos de conteúdo (texto e tools juntos);
// a OpenAI numera só as tool calls (0, 1, 2...). Por isso o mapa toolIdx.
func (p *Provider) ChatStream(ctx context.Context, c provider.Call, emit provider.Emit) (*openai.Usage, error) {
	if p.stream == nil {
		return nil, provider.Errorf(http.StatusNotImplemented, "unsupported", "streaming não configurado")
	}
	in, err := buildInput(c)
	if err != nil {
		return nil, err
	}
	events, err := p.stream.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId: in.ModelId, Messages: in.Messages, System: in.System,
		InferenceConfig: in.InferenceConfig, ToolConfig: in.ToolConfig,
	})
	if err != nil {
		return nil, mapError(err)
	}
	defer events.Close()

	created := time.Now().Unix()
	chunk := func(d openai.Delta, finish *string) openai.ChatChunk {
		return openai.ChatChunk{ID: c.ID, Object: "chat.completion.chunk", Created: created, Model: c.Alias,
			Choices: []openai.ChunkChoice{{Delta: d, FinishReason: finish}}}
	}
	toolIdx := map[int32]int{}
	var usage *openai.Usage

	for ev := range events.Events() {
		var out *openai.ChatChunk
		switch v := ev.(type) {
		case *types.ConverseStreamOutputMemberMessageStart:
			ch := chunk(openai.Delta{Role: "assistant"}, nil)
			out = &ch
		case *types.ConverseStreamOutputMemberContentBlockStart:
			if tu, ok := v.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
				idx := len(toolIdx)
				toolIdx[aws.ToInt32(v.Value.ContentBlockIndex)] = idx
				ch := chunk(openai.Delta{ToolCalls: []openai.ToolCall{{
					Index: &idx, ID: aws.ToString(tu.Value.ToolUseId), Type: "function",
					Function: openai.FunctionCall{Name: aws.ToString(tu.Value.Name)},
				}}}, nil)
				out = &ch
			}
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			switch d := v.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				ch := chunk(openai.Delta{Content: d.Value}, nil)
				out = &ch
			case *types.ContentBlockDeltaMemberToolUse:
				idx := toolIdx[aws.ToInt32(v.Value.ContentBlockIndex)]
				ch := chunk(openai.Delta{ToolCalls: []openai.ToolCall{{
					Index: &idx, Function: openai.FunctionCall{Arguments: aws.ToString(d.Value.Input)},
				}}}, nil)
				out = &ch
			}
		case *types.ConverseStreamOutputMemberMessageStop:
			reason := finishReason(v.Value.StopReason)
			ch := chunk(openai.Delta{}, &reason)
			out = &ch
		case *types.ConverseStreamOutputMemberMetadata:
			if u := v.Value.Usage; u != nil {
				in, o := int(aws.ToInt32(u.InputTokens)), int(aws.ToInt32(u.OutputTokens))
				usage = &openai.Usage{PromptTokens: in, CompletionTokens: o, TotalTokens: in + o}
				if cached := int(aws.ToInt32(u.CacheReadInputTokens)); cached > 0 {
					usage.PromptTokensDetails = &openai.PromptTokensDetails{CachedTokens: cached}
				}
			}
		}
		if out != nil {
			if err := emit(*out); err != nil {
				return usage, err
			}
		}
	}
	if err := events.Err(); err != nil {
		return usage, mapError(err)
	}
	return usage, nil
}
