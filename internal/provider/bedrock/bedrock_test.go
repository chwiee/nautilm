package bedrock

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

type fakeAPI struct {
	in  *bedrockruntime.ConverseInput
	out *bedrockruntime.ConverseOutput
	err error
}

func (f *fakeAPI) Converse(_ context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	f.in = in
	return f.out, f.err
}

func parse(t *testing.T, s string) *openai.ChatRequest {
	t.Helper()
	var r openai.ChatRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

// Uma conversa de tool calling completa, no formato OpenAI, precisa virar
// Converse válida: system separado, tool result dentro de user, papéis alternados.
func TestTraducaoIda(t *testing.T) {
	req := parse(t, `{
	  "model":"smart","max_tokens":100,
	  "messages":[
	    {"role":"system","content":"você é o kubets"},
	    {"role":"user","content":[{"type":"text","text":"pods?"}]},
	    {"role":"assistant","content":null,"tool_calls":[
	      {"id":"t1","type":"function","function":{"name":"list_pods","arguments":"{\"ns\":\"a\"}"}},
	      {"id":"t2","type":"function","function":{"name":"list_pods","arguments":"{\"ns\":\"b\"}"}}]},
	    {"role":"tool","tool_call_id":"t1","content":"2 pods"},
	    {"role":"tool","tool_call_id":"t2","content":"0 pods"}
	  ],
	  "tools":[{"type":"function","function":{"name":"list_pods","description":"lista","parameters":{"type":"object"}}}],
	  "tool_choice":"required"
	}`)
	in, err := buildInput(provider.Call{Model: "anthropic.claude", Request: req})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.System) != 1 {
		t.Errorf("system deveria ir separado: %d", len(in.System))
	}
	if len(in.Messages) != 3 {
		t.Fatalf("esperava user, assistant, user(2 tool results): %d mensagens", len(in.Messages))
	}
	if in.Messages[2].Role != types.ConversationRoleUser || len(in.Messages[2].Content) != 2 {
		t.Errorf("os dois tool results deviam fundir numa mensagem user")
	}
	if aws.ToInt32(in.InferenceConfig.MaxTokens) != 100 {
		t.Errorf("max_tokens não passou")
	}
	if _, ok := in.ToolConfig.ToolChoice.(*types.ToolChoiceMemberAny); !ok {
		t.Errorf("required deveria virar Any, veio %T", in.ToolConfig.ToolChoice)
	}
}

func TestTraducaoVolta(t *testing.T) {
	api := &fakeAPI{out: &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{Value: types.Message{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
					ToolUseId: aws.String("t9"), Name: aws.String("list_pods"),
					Input: document.NewLazyDocument(map[string]any{"ns": "x"}),
				}},
			},
		}},
		StopReason: types.StopReasonToolUse,
		Usage:      &types.TokenUsage{InputTokens: aws.Int32(50), OutputTokens: aws.Int32(7), TotalTokens: aws.Int32(57), CacheReadInputTokens: aws.Int32(40)},
	}}
	p := NewWithAPI(api)
	resp, err := p.Chat(context.Background(), provider.Call{Model: "m", Request: parse(t, `{"model":"smart","messages":[{"role":"user","content":"oi"}]}`), DefaultMaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	ch := resp.Choices[0]
	if ch.FinishReason != "tool_calls" || ch.Message.Content != nil {
		t.Errorf("finish=%q content=%v", ch.FinishReason, ch.Message.Content)
	}
	if tc := ch.Message.ToolCalls[0]; tc.ID != "t9" || tc.Function.Arguments != `{"ns":"x"}` {
		t.Errorf("tool call: %+v", tc)
	}
	if resp.Usage.TotalTokens != 57 || resp.Usage.PromptTokensDetails.CachedTokens != 40 {
		t.Errorf("usage: %+v", resp.Usage)
	}
	if aws.ToInt32(api.in.InferenceConfig.MaxTokens) != 256 {
		t.Errorf("max_tokens padrão do apelido não aplicado")
	}
}

func TestMapError(t *testing.T) {
	cases := map[string]int{"ThrottlingException": 429, "ValidationException": 400, "AccessDeniedException": 502}
	for code, want := range cases {
		err := mapError(&smithy.GenericAPIError{Code: code, Message: "x"})
		if got := err.(*provider.Error).Status; got != want {
			t.Errorf("%s → %d, quer %d", code, got, want)
		}
	}
}
