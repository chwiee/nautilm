package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
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

// A full tool-calling conversation in the OpenAI format must become a valid
// Converse input: separate system, tool results inside user, alternating roles.
func TestOutboundTranslation(t *testing.T) {
	req := parse(t, `{
	  "model":"smart","max_tokens":100,
	  "messages":[
	    {"role":"system","content":"you are kubets"},
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
		t.Errorf("system must go separately: %d", len(in.System))
	}
	if len(in.Messages) != 3 {
		t.Fatalf("expected user, assistant, user(2 tool results): %d messages", len(in.Messages))
	}
	if in.Messages[2].Role != types.ConversationRoleUser || len(in.Messages[2].Content) != 2 {
		t.Errorf("the two tool results must merge into one user message")
	}
	if aws.ToInt32(in.InferenceConfig.MaxTokens) != 100 {
		t.Errorf("max_tokens not forwarded")
	}
	if _, ok := in.ToolConfig.ToolChoice.(*types.ToolChoiceMemberAny); !ok {
		t.Errorf("required must become Any, got %T", in.ToolConfig.ToolChoice)
	}
}

func TestInboundTranslation(t *testing.T) {
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
		Usage:      &types.TokenUsage{InputTokens: aws.Int32(50), OutputTokens: aws.Int32(7), TotalTokens: aws.Int32(57), CacheReadInputTokens: aws.Int32(40), CacheWriteInputTokens: aws.Int32(10)},
	}}
	p := NewWithAPI(api, nil)
	resp, err := p.Chat(context.Background(), provider.Call{Model: "m", Request: parse(t, `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`), DefaultMaxTokens: 256})
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
	// Normalized (OpenAI semantics): input = 50 + 40 cache read + 10 cache write.
	if resp.Usage.PromptTokens != 100 || resp.Usage.TotalTokens != 107 || resp.Usage.CacheRead() != 40 || resp.Usage.CacheWrite() != 10 {
		t.Errorf("usage: %+v", resp.Usage)
	}
	if aws.ToInt32(api.in.InferenceConfig.MaxTokens) != 256 {
		t.Errorf("the alias default max_tokens was not applied")
	}
}

func TestMapError(t *testing.T) {
	cases := map[string]int{"ThrottlingException": 429, "ValidationException": 400, "AccessDeniedException": 502}
	for code, want := range cases {
		err := mapError(&smithy.GenericAPIError{Code: code, Message: "x"}, "m")
		if got := err.(*provider.Error).Status; got != want {
			t.Errorf("%s → %d, want %d", code, got, want)
		}
	}
}

// The AWS account daily quota is a provider limit, not nautilm's: own code,
// a message that says so, and the SDK must not retry it.
func TestDailyQuotaIsAProviderError(t *testing.T) {
	daily := &smithy.GenericAPIError{Code: "ThrottlingException", Message: "Too many tokens per day, please wait before trying again."}
	err := mapError(daily, "google.gemma-3-4b-it")
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Status != 429 || pe.Code != CodeProviderQuotaDay {
		t.Fatalf("daily quota: %v", err)
	}
	for _, want := range []string{"AWS account", "google.gemma-3-4b-it", "not a nautilm quota", "Too many tokens per day"} {
		if !strings.Contains(pe.Message, want) {
			t.Errorf("message must contain %q: %s", want, pe.Message)
		}
	}
	if noRetryOnDailyQuota(daily) != aws.FalseTernary {
		t.Error("a daily quota must not be retried by the SDK")
	}

	// A per-minute throttle is still a normal 429 that the SDK may retry.
	minute := &smithy.GenericAPIError{Code: "ThrottlingException", Message: "Too many requests, please wait before trying again."}
	if err := mapError(minute, "m"); !errors.As(err, &pe) || pe.Code != "ThrottlingException" {
		t.Errorf("per-minute throttle: %v", err)
	}
	if noRetryOnDailyQuota(minute) != aws.UnknownTernary {
		t.Error("a per-minute throttle keeps the default retry behavior")
	}
}

// kinds lists the block kinds in order ("text", "cache", "toolResult"...).
func kinds[T any](blocks []T) string {
	var out []string
	for _, b := range blocks {
		switch any(b).(type) {
		case *types.SystemContentBlockMemberText, *types.ContentBlockMemberText:
			out = append(out, "text")
		case *types.SystemContentBlockMemberCachePoint, *types.ContentBlockMemberCachePoint:
			out = append(out, "cache")
		case *types.ContentBlockMemberToolResult:
			out = append(out, "toolResult")
		case *types.ContentBlockMemberToolUse:
			out = append(out, "toolUse")
		default:
			out = append(out, fmt.Sprintf("%T", b))
		}
	}
	return strings.Join(out, ",")
}

// The agent's cache_control markers (Anthropic/OpenRouter convention) become
// cachePoint blocks at the same place: this is what keeps the MidTS CAG.
func TestPromptCacheMarkers(t *testing.T) {
	req := parse(t, `{"model":"smart","messages":[
	  {"role":"system","content":[{"type":"text","text":"rules"},{"type":"text","text":"long guides","cache_control":{"type":"ephemeral"}}]},
	  {"role":"system","content":"memory summary"},
	  {"role":"user","content":[{"type":"text","text":"old question","cache_control":{"type":"ephemeral"}}]},
	  {"role":"assistant","content":"answer","cache_control":{"type":"ephemeral"}},
	  {"role":"user","content":"new question"}]}`)

	in, err := buildInput(provider.Call{Model: "anthropic.claude", Request: req, PromptCache: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(in.System); got != "text,text,cache,text" {
		t.Errorf("system: %s", got)
	}
	if got := kinds(in.Messages[0].Content); got != "text,cache" {
		t.Errorf("user part marker: %s", got)
	}
	if got := kinds(in.Messages[1].Content); got != "text,cache" {
		t.Errorf("message-level marker: %s", got)
	}
	if got := kinds(in.Messages[2].Content); got != "text" {
		t.Errorf("no marker: %s", got)
	}

	// off (default): markers are ignored, the text is the same.
	in, _ = buildInput(provider.Call{Model: "qwen", Request: req})
	if got := kinds(in.System) + "|" + kinds(in.Messages[0].Content); got != "text,text,text|text" {
		t.Errorf("off must drop every cachePoint: %s", got)
	}
}

func TestPromptCacheAutoAndLimit(t *testing.T) {
	plain := parse(t, `{"model":"smart","messages":[{"role":"system","content":"guides"},{"role":"user","content":"hi"}]}`)
	in, _ := buildInput(provider.Call{Model: "anthropic.claude", Request: plain, PromptCache: "auto"})
	if got := kinds(in.System); got != "text,cache" {
		t.Errorf("auto must cache the system prompt: %s", got)
	}
	in, _ = buildInput(provider.Call{Model: "anthropic.claude", Request: plain, PromptCache: "explicit"})
	if got := kinds(in.System); got != "text" {
		t.Errorf("explicit without markers adds nothing: %s", got)
	}

	// Six markers: Bedrock accepts 4. The first 4 stay (most stable prefix).
	var msgs []string
	for i := 0; i < 6; i++ {
		role := map[bool]string{true: "user", false: "assistant"}[i%2 == 0]
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":[{"type":"text","text":"m%d","cache_control":{"type":"ephemeral"}}]}`, role, i))
	}
	many := parse(t, `{"model":"smart","messages":[`+strings.Join(msgs, ",")+`,{"role":"user","content":"last"}]}`)
	in, _ = buildInput(provider.Call{Model: "anthropic.claude", Request: many, PromptCache: "auto"})
	n := 0
	for _, m := range in.Messages {
		n += strings.Count(kinds(m.Content), "cache")
	}
	if n != maxCachePoints || kinds(in.Messages[4].Content) != "text" {
		t.Errorf("cache points %d (want %d, first ones kept)", n, maxCachePoints)
	}
	if kinds(in.System) != "" {
		t.Error("auto must not add a point when the agent sent markers")
	}
}

// A marker on a tool result goes right after the toolResult block.
func TestPromptCacheAfterToolResult(t *testing.T) {
	req := parse(t, `{"model":"smart","messages":[
	  {"role":"user","content":"pods?"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"list","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"t1","content":[{"type":"text","text":"big log","cache_control":{"type":"ephemeral"}}]}]}`)
	in, err := buildInput(provider.Call{Model: "anthropic.claude", Request: req, PromptCache: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(in.Messages[2].Content); got != "toolResult,cache" {
		t.Errorf("tool result marker: %s", got)
	}
}
