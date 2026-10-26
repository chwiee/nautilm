package bedrock

import (
	"context"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// EventReader is what the SDK returns from ConverseStreamOutput.GetStream().
// Our own interface because the SDK field is private: this way it is testable.
type EventReader interface {
	Events() <-chan types.ConverseStreamOutput
	Close() error
	Err() error
}

// StreamAPI opens a ConverseStream.
type StreamAPI interface {
	ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput) (EventReader, error)
}

// sdkStream adapts the SDK client to StreamAPI.
type sdkStream struct{ c *bedrockruntime.Client }

func (s sdkStream) ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput) (EventReader, error) {
	out, err := s.c.ConverseStream(ctx, in)
	if err != nil {
		return nil, err
	}
	return out.GetStream(), nil
}

// ChatStream translates ConverseStream events into OpenAI chunks:
//
//	messageStart                → delta {role: assistant}
//	contentBlockDelta (text)    → delta {content}
//	contentBlockStart (toolUse) → delta {tool_calls: [{index, id, name, arguments: ""}]}
//	contentBlockDelta (toolUse) → delta {tool_calls: [{index, arguments: "<piece of the JSON>"}]}
//	messageStop                 → finish_reason
//	metadata                    → usage (returned to the gateway)
//
// Detail: Bedrock numbers content blocks (text and tools together);
// OpenAI numbers only tool calls (0, 1, 2...). Hence the toolIdx map.
func (p *Provider) ChatStream(ctx context.Context, c provider.Call, emit provider.Emit) (*openai.Usage, error) {
	if p.stream == nil {
		return nil, provider.Errorf(http.StatusNotImplemented, "unsupported", "streaming not configured")
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
		return nil, mapError(err, c.Model)
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
			usage = toUsage(v.Value.Usage)
		}
		if out != nil {
			if err := emit(*out); err != nil {
				return usage, err
			}
		}
	}
	if err := events.Err(); err != nil {
		return usage, mapError(err, c.Model)
	}
	return usage, nil
}
