package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chwiee/pedagio/internal/auth"
	"github.com/chwiee/pedagio/internal/config"
	"github.com/chwiee/pedagio/internal/metrics"
	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

// fakeStreamer emite os textos em ordem. Se gate != nil, espera nele antes
// de cada chunk depois do primeiro (para provar que o 1º chega antes do fim).
type fakeStreamer struct {
	fake
	texts    []string
	openErr  error // falha antes de emitir
	midErr   error // falha depois do 1º chunk
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
			return nil, f.midErr
		}
	}
	stop := "stop"
	emit(openai.ChatChunk{ID: c.ID, Model: c.Alias, Choices: []openai.ChunkChoice{{FinishReason: &stop}}})
	return &openai.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6}, nil
}

func setupP(t *testing.T, a, b provider.Provider) (*httptest.Server, *metrics.Registry) {
	t.Helper()
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	gw := New(cfg, map[string]provider.Provider{"a": a, "b": b}, auth.Chain{auth.NewTokens(cfg.Auth.Tokens)}, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return srv, m
}

const streamBody = `{"model":"smart","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"oi"}]}`

// readSSE devolve os payloads "data:" em ordem.
func readSSE(t *testing.T, r io.Reader) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, d)
		}
	}
	return out
}

func TestStreamReal(t *testing.T) {
	a := &fakeStreamer{texts: []string{"o", "i"}}
	srv, m := setupP(t, a, &fake{})
	resp := post(t, srv, "tok-kubets", streamBody)
	if resp.Header.Get("X-Pedagio-Model") != "modelo-grande" || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("headers: %v", resp.Header)
	}
	events := readSSE(t, resp.Body)
	if events[len(events)-1] != "[DONE]" {
		t.Errorf("último evento deveria ser [DONE]: %v", events)
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
	if text != "oi" || usage == nil || usage.TotalTokens != 6 {
		t.Errorf("text=%q usage=%+v", text, usage)
	}
	if a.streamed != 1 || len(a.calls) != 0 {
		t.Errorf("deveria usar ChatStream, não Chat")
	}
	var sb strings.Builder
	m.Write(&sb)
	if !strings.Contains(sb.String(), "pedagio_time_to_first_token_seconds_count") {
		t.Errorf("métrica de TTFT ausente")
	}
}

// O 1º chunk precisa chegar ao agente ENQUANTO o provider ainda gera o resto.
// É a diferença entre stream de verdade e simulado.
func TestPrimeiroChunkChegaAntesDoFim(t *testing.T) {
	gate := make(chan struct{})
	a := &fakeStreamer{texts: []string{"primeiro", "segundo"}, gate: gate}
	srv, _ := setupP(t, a, &fake{})
	resp := post(t, srv, "tok-kubets", streamBody)

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
		if !strings.Contains(line, "primeiro") {
			t.Errorf("1º evento: %s", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("1º chunk não chegou enquanto o provider estava parado: o gateway está segurando em buffer")
	}
	close(gate)
}

func TestStreamFallbackAntesDoPrimeiroByte(t *testing.T) {
	a := &fakeStreamer{openErr: provider.Errorf(429, "ThrottlingException", "devagar")}
	b := &fakeStreamer{texts: []string{"do fast"}}
	srv, _ := setupP(t, a, b)
	resp := post(t, srv, "tok-kubets", streamBody)
	if resp.StatusCode != 200 || resp.Header.Get("X-Pedagio-Alias") != "fast" {
		t.Fatalf("status %d alias %q", resp.StatusCode, resp.Header.Get("X-Pedagio-Alias"))
	}
	if events := readSSE(t, resp.Body); !strings.Contains(strings.Join(events, ""), "do fast") {
		t.Errorf("eventos: %v", events)
	}
}

func TestStreamErroNoMeioNaoFazFallback(t *testing.T) {
	a := &fakeStreamer{texts: []string{"come", "ço"}, midErr: provider.Errorf(503, "ModelStreamErrorException", "caiu")}
	b := &fakeStreamer{texts: []string{"nunca"}}
	srv, _ := setupP(t, a, b)
	resp := post(t, srv, "tok-kubets", streamBody)
	if resp.StatusCode != 200 {
		t.Fatalf("o 200 já tinha saído: status %d", resp.StatusCode)
	}
	events := readSSE(t, resp.Body)
	last := events[len(events)-1]
	if !strings.Contains(last, `"error"`) || !strings.Contains(last, "caiu") {
		t.Errorf("último evento deveria ser o erro: %v", events)
	}
	if b.streamed != 0 {
		t.Errorf("não pode trocar de modelo no meio do stream")
	}
}

func TestStreamErroSemFallbackViraHTTP(t *testing.T) {
	a := &fakeStreamer{openErr: provider.Errorf(400, "ValidationException", "pedido ruim")}
	srv, _ := setupP(t, a, &fakeStreamer{})
	resp := post(t, srv, "tok-kubets", streamBody)
	if resp.StatusCode != 400 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("antes do 1º byte o erro deve ser HTTP normal: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
