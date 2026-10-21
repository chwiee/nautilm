package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

type fakeReader struct {
	ch  chan types.ConverseStreamOutput
	err error
}

func (f *fakeReader) Events() <-chan types.ConverseStreamOutput { return f.ch }
func (f *fakeReader) Close() error                              { return nil }
func (f *fakeReader) Err() error                                { return f.err }

type fakeStream struct {
	in     *bedrockruntime.ConverseStreamInput
	events []types.ConverseStreamOutput
	err    error // erro no meio (via Err())
	open   error // erro ao abrir
}

func (f *fakeStream) ConverseStream(_ context.Context, in *bedrockruntime.ConverseStreamInput) (EventReader, error) {
	f.in = in
	if f.open != nil {
		return nil, f.open
	}
	ch := make(chan types.ConverseStreamOutput, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	close(ch)
	return &fakeReader{ch: ch, err: f.err}, nil
}

func idx(i int32) *int32 { return &i }

// Texto + uma tool call no bloco 1: a OpenAI precisa ver tool_calls[0].
func TestStreamTraduzEventos(t *testing.T) {
	fs := &fakeStream{events: []types.ConverseStreamOutput{
		&types.ConverseStreamOutputMemberMessageStart{Value: types.MessageStartEvent{Role: types.ConversationRoleAssistant}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: idx(0), Delta: &types.ContentBlockDeltaMemberText{Value: "Vou "}}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: idx(0), Delta: &types.ContentBlockDeltaMemberText{Value: "olhar."}}},
		&types.ConverseStreamOutputMemberContentBlockStart{Value: types.ContentBlockStartEvent{ContentBlockIndex: idx(1), Start: &types.ContentBlockStartMemberToolUse{Value: types.ToolUseBlockStart{ToolUseId: aws.String("t1"), Name: aws.String("list_pods")}}}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: idx(1), Delta: &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: aws.String(`{"ns":`)}}}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: idx(1), Delta: &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: aws.String(`"loja"}`)}}}},
		&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonToolUse}},
		&types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{Usage: &types.TokenUsage{InputTokens: aws.Int32(30), OutputTokens: aws.Int32(9), TotalTokens: aws.Int32(39)}}},
	}}
	p := NewWithAPI(nil, fs)

	var chunks []openai.ChatChunk
	usage, err := p.ChatStream(context.Background(), provider.Call{Model: "m", ID: "chatcmpl-x", Alias: "smart",
		Request: parse(t, `{"model":"smart","messages":[{"role":"user","content":"oi"}]}`)},
		func(v any) error { chunks = append(chunks, v.(openai.ChatChunk)); return nil })
	if err != nil {
		t.Fatal(err)
	}

	var text, args, name string
	var finish string
	for _, c := range chunks {
		if c.ID != "chatcmpl-x" || c.Model != "smart" {
			t.Errorf("chunk com id/model errado: %s %s", c.ID, c.Model)
		}
		d := c.Choices[0].Delta
		text += d.Content
		for _, tc := range d.ToolCalls {
			if *tc.Index != 0 {
				t.Errorf("1ª tool deve ter index 0 na OpenAI, veio %d", *tc.Index)
			}
			name += tc.Function.Name
			args += tc.Function.Arguments
		}
		if c.Choices[0].FinishReason != nil {
			finish = *c.Choices[0].FinishReason
		}
	}
	if text != "Vou olhar." || name != "list_pods" || args != `{"ns":"loja"}` || finish != "tool_calls" {
		t.Errorf("text=%q name=%q args=%q finish=%q", text, name, args, finish)
	}
	if !json.Valid([]byte(args)) {
		t.Errorf("argumentos remontados não são JSON")
	}
	if usage == nil || usage.TotalTokens != 39 {
		t.Errorf("usage: %+v", usage)
	}
}

func TestStreamErros(t *testing.T) {
	req := parse(t, `{"model":"smart","messages":[{"role":"user","content":"oi"}]}`)
	noop := func(any) error { return nil }

	// Erro ao abrir: vira *provider.Error retryable (gateway pode fazer fallback).
	p := NewWithAPI(nil, &fakeStream{open: &smithy.GenericAPIError{Code: "ThrottlingException"}})
	_, err := p.ChatStream(context.Background(), provider.Call{Model: "m", Request: req}, noop)
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Status != 429 {
		t.Errorf("abrir: %v", err)
	}

	// Erro no meio (exceção no event stream).
	p = NewWithAPI(nil, &fakeStream{err: &smithy.GenericAPIError{Code: "ModelStreamErrorException"},
		events: []types.ConverseStreamOutput{&types.ConverseStreamOutputMemberMessageStart{}}})
	_, err = p.ChatStream(context.Background(), provider.Call{Model: "m", Request: req}, noop)
	if !errors.As(err, &pe) || pe.Status != 502 {
		t.Errorf("meio: %v", err)
	}
}
