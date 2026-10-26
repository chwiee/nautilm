package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// fakeStreamer emits the texts in order. If gate != nil, it waits on it
// before every chunk after the first (to prove the 1st arrives before the end).
type fakeStreamer struct {
	fake
	texts    []string
	openErr  error // fails before emitting
	midErr   error // fails after the 1st chunk
	gate     chan struct{}
	streamed int
}

func (f *fakeStreamer) ChatStream(ctx context.Context, c provider.Call, emit provider.Emit) (*openai.Usage, error) {
	f.streamed++
	if f.openErr != nil {
		return nil, f.openErr
	}
	for i, s := range f.texts {
		if i > 0 && f.gate != nil {
			select {
			case <-f.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err := emit(openai.ChatChunk{ID: c.ID, Object: "chat.completion.chunk", Model: c.Alias,
			Choices: []openai.ChunkChoice{{Delta: openai.Delta{Content: s}}}}); err != nil {
			return nil, err
		}
		if f.midErr != nil {
			// tokens were spent before it broke: they must still be billed
			return &openai.Usage{PromptTokens: 4, CompletionTokens: 1, TotalTokens: 5}, f.midErr
		}
	}
	stop := "stop"
	emit(openai.ChatChunk{ID: c.ID, Model: c.Alias, Choices: []openai.ChunkChoice{{FinishReason: &stop}}})
	return &openai.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6}, nil
}

const streamBody = `{"model":"smart","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`

func TestRealStream(t *testing.T) {
	a := &fakeStreamer{texts: []string{"h", "i"}}
	env := setup(t, a, &fake{})
	resp := post(t, env.srv, "tok-k8s", streamBody)
	if resp.Header.Get("X-Nautilm-Model") != "big-model" || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("headers: %v", resp.Header)
	}
	events := readSSE(t, resp.Body)
	if events[len(events)-1] != "[DONE]" {
		t.Errorf("the last event must be [DONE]: %v", events)
	}
	var text string
	var usage *openai.Usage
	for _, e := range events[:len(events)-1] {
		var c openai.ChatChunk
		json.Unmarshal([]byte(e), &c)
		if c.Model != "smart" || !strings.HasPrefix(c.ID, "chatcmpl-") {
			t.Errorf("chunk id/model: %s", e)
		}
		for _, ch := range c.Choices {
			text += ch.Delta.Content
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	if text != "hi" || usage == nil || usage.TotalTokens != 6 {
		t.Errorf("text=%q usage=%+v", text, usage)
	}
	if a.streamed != 1 || len(a.calls) != 0 {
		t.Errorf("must use ChatStream, not Chat")
	}
	if !strings.Contains(metricsText(env.metrics), "nautilm_time_to_first_token_seconds_count") {
		t.Errorf("TTFT metric missing")
	}
}

// The 1st chunk must reach the agent WHILE the provider is still generating.
// That is the difference between real and simulated streaming.
func TestFirstChunkArrivesBeforeTheEnd(t *testing.T) {
	gate := make(chan struct{})
	a := &fakeStreamer{texts: []string{"first", "second"}, gate: gate}
	env := setup(t, a, &fake{})
	resp := post(t, env.srv, "tok-k8s", streamBody)

	first := make(chan string, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				first <- line
				io.Copy(io.Discard, br)
				return
			}
		}
	}()
	select {
	case line := <-first:
		if !strings.Contains(line, "first") {
			t.Errorf("1st event: %s", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the 1st chunk did not arrive while the provider was paused: the gateway is buffering")
	}
	close(gate)
}

func TestStreamFallbackBeforeTheFirstByte(t *testing.T) {
	a := &fakeStreamer{openErr: provider.Errorf(429, "ThrottlingException", "slow down")}
	b := &fakeStreamer{texts: []string{"from fast"}}
	env := setup(t, a, b)
	resp := post(t, env.srv, "tok-k8s", streamBody)
	if resp.StatusCode != 200 || resp.Header.Get("X-Nautilm-Alias") != "fast" {
		t.Fatalf("status %d alias %q", resp.StatusCode, resp.Header.Get("X-Nautilm-Alias"))
	}
	if events := readSSE(t, resp.Body); !strings.Contains(strings.Join(events, ""), "from fast") {
		t.Errorf("events: %v", events)
	}
}

func TestStreamErrorMidwayDoesNotFallBack(t *testing.T) {
	a := &fakeStreamer{texts: []string{"sta", "rt"}, midErr: provider.Errorf(503, "ModelStreamErrorException", "broke")}
	b := &fakeStreamer{texts: []string{"never"}}
	env := setup(t, a, b)
	resp := post(t, env.srv, "tok-k8s", streamBody)
	if resp.StatusCode != 200 {
		t.Fatalf("the 200 was already sent: status %d", resp.StatusCode)
	}
	events := readSSE(t, resp.Body)
	last := events[len(events)-1]
	if !strings.Contains(last, `"error"`) || !strings.Contains(last, "broke") {
		t.Errorf("the last event must be the error: %v", events)
	}
	if b.streamed != 0 {
		t.Errorf("must not switch models in the middle of a stream")
	}
	// The tokens spent before the break are billed.
	if !strings.Contains(metricsText(env.metrics), `nautilm_tokens_total{agent="k8s",alias="smart",provider="a",model="big-model",kind="input"} 4`) {
		t.Errorf("tokens of a broken stream must be accounted:\n%s", metricsText(env.metrics))
	}
}

func TestStreamErrorWithoutFallbackIsHTTP(t *testing.T) {
	a := &fakeStreamer{openErr: provider.Errorf(400, "ValidationException", "bad request")}
	env := setup(t, a, &fakeStreamer{})
	resp := post(t, env.srv, "tok-k8s", streamBody)
	if resp.StatusCode != 400 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("before the 1st byte the error must be a normal HTTP error: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
