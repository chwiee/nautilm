package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/chwiee/nautilm/internal/config"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// spy keeps what the provider RECEIVED: that is what the model would see.
type spy struct {
	call  provider.Call
	input []string
}

func (s *spy) Chat(_ context.Context, c provider.Call) (*openai.ChatResponse, error) {
	s.call = c
	txt := "ok"
	return &openai.ChatResponse{Choices: []openai.Choice{{Message: openai.ResponseMessage{Role: "assistant", Content: &txt}}}}, nil
}

func (s *spy) Embed(_ context.Context, c provider.EmbedCall) ([][]float32, *openai.Usage, error) {
	s.input = append([]string(nil), c.Input...)
	out := make([][]float32, len(c.Input))
	for i := range out {
		out[i] = []float32{1}
	}
	return out, &openai.Usage{}, nil
}

func setupSpy(t *testing.T, extra string) (testEnv, *spy) {
	t.Helper()
	s := &spy{}
	return setupCfg(t, testConfig+extra, map[string]provider.Provider{"a": s, "b": &fake{}}), s
}

const chatWithSecrets = `{"model":"smart","messages":[
 {"role":"system","content":"you help the team"},
 {"role":"user","content":[{"type":"text","text":"my cpf is 123.456.789-09 and senha=hunter2"}]},
 {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"logs","arguments":"{\"token\":\"abc123secret\"}"}}]},
 {"role":"tool","tool_call_id":"c1","content":"connecting with AKIAABCDEFGHIJKLMNOP as ana@company.com"}
],"tools":[{"type":"function","function":{"name":"logs","parameters":{"type":"object","properties":{"token":{"type":"string"}}}}}]}`

func TestChatMasksBeforeTheProvider(t *testing.T) {
	env, s := setupSpy(t, "")
	resp := post(t, env.srv, "tok-k8s", chatWithSecrets)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}

	// 1) What goes to the openaicompat provider (raw map)
	raw, _ := json.Marshal(s.call.Raw)
	// 2) What goes to Bedrock (struct)
	st, _ := json.Marshal(s.call.Request.Messages)
	for name, got := range map[string]string{"raw": string(raw), "struct": string(st)} {
		for _, leak := range []string{"123.456.789-09", "hunter2", "abc123secret", "AKIAABCDEFGHIJKLMNOP", "ana@company.com"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: leaked %q", name, leak)
			}
		}
		for _, mark := range []string{"[CPF]", "[SECRET]", "[AWS_KEY]", "[EMAIL]"} {
			if !strings.Contains(got, mark) {
				t.Errorf("%s: missing %s in %s", name, mark, got)
			}
		}
	}
	// Structure intact: tool_call_id, the tool definition and its "token" parameter.
	if s.call.Request.Messages[3].ToolCallID != "c1" || len(s.call.Request.Tools) != 1 {
		t.Errorf("structure changed: %+v", s.call.Request.Messages[3])
	}
	tools, _ := json.Marshal(s.call.Raw["tools"])
	if !strings.Contains(string(tools), `"token":{"type":"string"}`) {
		t.Error("tool definitions must not be masked")
	}
	// The agent is told, the metric counts (never the value).
	if h := resp.Header.Get("X-Nautilm-Masked"); h != "aws_key=1,cpf=1,email=1,secret=2" {
		t.Errorf("header = %q", h)
	}
	if !strings.Contains(metricsText(env.metrics), `nautilm_masked_total{agent="k8s",rule="secret"} 2`) {
		t.Errorf("metric:\n%s", metricsText(env.metrics))
	}
}

func TestChatWithoutSensitiveDataIsUntouched(t *testing.T) {
	env, s := setupSpy(t, "")
	resp := post(t, env.srv, "tok-k8s", `{"model":"smart","messages":[{"role":"user","content":"pod checkout in CrashLoopBackOff"}]}`)
	if resp.Header.Get("X-Nautilm-Masked") != "" || s.call.Request.Messages[0].Content.Text != "pod checkout in CrashLoopBackOff" {
		t.Errorf("must not change: %q", s.call.Request.Messages[0].Content.Text)
	}
}

func TestEmbeddingsAreMasked(t *testing.T) {
	env, s := setupSpy(t, "")
	postEmbed(t, env, "tok-k8s", `{"model":"embed","input":["customer 123.456.789-09","normal"]}`)
	if s.input[0] != "customer [CPF]" || s.input[1] != "normal" {
		t.Errorf("input = %q", s.input)
	}
}

func TestMaskingIsConfigurable(t *testing.T) {
	env, s := setupSpy(t, "masking: {disable: [email], extra: [{name: employee_id, regex: 'MAT-\\d{6}'}]}\n")
	post(t, env.srv, "tok-k8s", fmt.Sprintf(`{"model":"smart","messages":[{"role":"user","content":%q}]}`, "ana@x.com MAT-123456"))
	if got := s.call.Request.Messages[0].Content.Text; got != "ana@x.com [EMPLOYEE_ID]" {
		t.Errorf("got %q", got)
	}

	env, s = setupSpy(t, "masking: {disabled: true}\n")
	post(t, env.srv, "tok-k8s", `{"model":"smart","messages":[{"role":"user","content":"123.456.789-09"}]}`)
	if got := s.call.Request.Messages[0].Content.Text; got != "123.456.789-09" {
		t.Errorf("disabled changed it: %q", got)
	}

	if _, err := config.Parse([]byte(testConfig + "masking: {disable: [cpff]}\n")); err == nil || !strings.Contains(err.Error(), "cpff") {
		t.Errorf("the config error must name the rule: %v", err)
	}
}
