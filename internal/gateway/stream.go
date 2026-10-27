package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/chwiee/nautilm/internal/auth"
	"github.com/chwiee/nautilm/internal/live"
	"github.com/chwiee/nautilm/internal/pricing"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// streamResult sums up what happened in a streaming request, for the log.
type streamResult struct {
	used    served
	usage   *openai.Usage
	cost    pricing.Result
	mode    string        // "real" (the provider streams) or "simulated"
	ttft    time.Duration // time until the 1st chunk reached the agent
	started bool          // did we already send bytes to the agent?
	err     error
}

// streamChain is callChain for streaming. The important difference:
//
//	fallback is only possible BEFORE the first byte reaches the agent.
//
// While the provider has emitted nothing (connection error, 429, 5xx when
// opening), we try the next alias as usual. After the first chunk, the HTTP
// 200 status is already sent: an error becomes a "data: {"error": ...}"
// event and the stream ends.
func (g *Gateway) streamChain(ctx context.Context, st *live.State, w http.ResponseWriter, id auth.Identity, b billing, req *openai.ChatRequest,
	raw map[string]any, h http.Header, callID string, includeUsage bool, trace string) streamResult {

	chain := st.Cfg.Chain(req.Model)
	start := time.Now()
	var res streamResult
	for i, alias := range chain {
		m := st.Cfg.Models[alias]
		if i > 0 {
			g.metrics.Fallback(b.agent, chain[i-1], alias)
			g.log.Warn("fallback", "agent", id.Agent, "from", chain[i-1], "to", alias, "error", res.err, "stream", true)
		}
		used := served{alias: alias, provider: m.Provider, model: m.Model}
		sw := &sseWriter{w: w, onStart: func() {
			setServedHeaders(w, used)
			res.ttft = time.Since(start)
		}}
		call := provider.Call{Model: m.Model, Request: req, Raw: raw, DefaultMaxTokens: m.MaxTokens,
			Header: h, ID: callID, Alias: req.Model}

		t0 := time.Now()
		var u *openai.Usage
		var err error
		if s, ok := st.Providers[m.Provider].(provider.Streamer); ok {
			res.mode = "real"
			u, err = s.ChatStream(ctx, call, sw.emit)
		} else {
			res.mode = "simulated"
			u, err = simulate(ctx, st.Providers[m.Provider], call, sw.emit)
		}

		status := http.StatusOK
		if err != nil {
			status = asProviderError(err).Status
		}
		// Tokens spent in a stream that broke in the middle are still billed.
		res.cost = g.record(ctx, st, b, "chat", used, status, time.Since(t0), u, trace)
		if sw.started {
			g.metrics.FirstToken(b.agent, alias, m.Model, res.ttft)
		}
		res.used, res.usage, res.started, res.err = used, u, sw.started, err

		if err == nil {
			if !sw.started {
				// The provider finished without emitting anything (empty
				// answer): the agent still needs a valid stream.
				sw.start()
			}
			if includeUsage && u != nil {
				sw.emit(openai.ChatChunk{ID: callID, Object: "chat.completion.chunk", Created: time.Now().Unix(),
					Model: req.Model, Choices: []openai.ChunkChoice{}, Usage: u})
			}
			sw.done()
			return res
		}
		if sw.started {
			perr := asProviderError(err)
			sw.emit(openai.ErrorResponse{Error: openai.ErrorBody{Message: perr.Message, Type: "api_error", Code: perr.Code}})
			return res
		}
		if !asProviderError(err).Retryable() || ctx.Err() != nil {
			return res
		}
	}
	return res
}

// simulate serves providers without streaming: it waits for the whole
// answer and sends it in chunks. It works with any client; only the time to
// the first token does not improve.
func simulate(ctx context.Context, p provider.Provider, c provider.Call, emit provider.Emit) (*openai.Usage, error) {
	resp, err := p.Chat(ctx, c)
	if err != nil {
		return nil, err
	}
	created := time.Now().Unix()
	for _, ch := range resp.Choices {
		d := openai.Delta{Role: "assistant"}
		if ch.Message.Content != nil {
			d.Content = *ch.Message.Content
		}
		for i, tc := range ch.Message.ToolCalls {
			idx := i
			tc.Index = &idx
			d.ToolCalls = append(d.ToolCalls, tc)
		}
		reason := ch.FinishReason
		for _, chunk := range []openai.ChunkChoice{
			{Index: ch.Index, Delta: d},
			{Index: ch.Index, Delta: openai.Delta{}, FinishReason: &reason},
		} {
			if err := emit(openai.ChatChunk{ID: c.ID, Object: "chat.completion.chunk", Created: created,
				Model: c.Alias, Choices: []openai.ChunkChoice{chunk}}); err != nil {
				return resp.Usage, err
			}
		}
	}
	return resp.Usage, nil
}

// sseWriter writes SSE events. The headers (200 + X-Nautilm-*) only go out
// on the first emit: until then the gateway can still switch models or
// return a normal HTTP error.
type sseWriter struct {
	w       http.ResponseWriter
	onStart func()
	started bool
}

func (s *sseWriter) start() {
	if s.started {
		return
	}
	s.started = true
	if s.onStart != nil {
		s.onStart()
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx/ingress: do not buffer the stream
	s.w.WriteHeader(http.StatusOK)
}

func (s *sseWriter) emit(v any) error {
	s.start()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err // the agent disconnected
	}
	s.flush()
	return nil
}

func (s *sseWriter) done() {
	io.WriteString(s.w, "data: [DONE]\n\n")
	s.flush()
}

func (s *sseWriter) flush() {
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}
