package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/chwiee/pedagio/internal/auth"
	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

// streamResult resume o que aconteceu num pedido com stream, para log.
type streamResult struct {
	used    served
	usage   *openai.Usage
	mode    string        // "real" (provider faz streaming) ou "simulado"
	ttft    time.Duration // tempo até o 1º chunk chegar ao agente
	started bool          // já mandamos bytes ao agente?
	err     error
}

// streamChain é o callChain do streaming. A diferença importante:
//
//	fallback só é possível ANTES do primeiro byte ir para o agente.
//
// Enquanto o provider não emitiu nada (erro de conexão, 429, 5xx na
// abertura), tentamos o próximo apelido normalmente. Depois que o primeiro
// chunk saiu, o status HTTP 200 já foi enviado: um erro vira um evento
// "data: {"error": ...}" e o stream termina.
func (g *Gateway) streamChain(ctx context.Context, w http.ResponseWriter, id auth.Identity, req *openai.ChatRequest,
	raw map[string]any, h http.Header, callID string, includeUsage bool) streamResult {

	chain := g.cfg.Chain(req.Model)
	start := time.Now()
	var res streamResult
	for i, alias := range chain {
		m := g.cfg.Models[alias]
		if i > 0 {
			g.metrics.Fallback(id.Agent, chain[i-1], alias)
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
		if s, ok := g.providers[m.Provider].(provider.Streamer); ok {
			res.mode = "real"
			u, err = s.ChatStream(ctx, call, sw.emit)
		} else {
			res.mode = "simulado"
			u, err = simulate(ctx, g.providers[m.Provider], call, sw.emit)
		}

		status := http.StatusOK
		if err != nil {
			status = asProviderError(err).Status
		}
		in, out, cached := usage(u)
		g.metrics.Call(id.Agent, alias, m.Model, status, time.Since(t0), in, out, cached)
		if sw.started {
			g.metrics.FirstToken(id.Agent, alias, m.Model, res.ttft)
		}
		res.used, res.usage, res.started, res.err = used, u, sw.started, err

		if err == nil {
			if !sw.started {
				// Provider terminou sem emitir nada (resposta vazia): ainda
				// assim o agente precisa de um stream válido.
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

// simulate serve providers sem streaming: espera a resposta inteira e
// manda em chunks. Funciona com qualquer cliente; só o tempo até o 1º
// token é que não melhora.
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

// sseWriter escreve eventos SSE. Os headers (200 + X-Pedagio-*) só saem no
// primeiro emit — até lá, o gateway ainda pode trocar de modelo ou devolver
// um erro HTTP normal.
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
	h.Set("X-Accel-Buffering", "no") // nginx/ingress: não segurar o stream em buffer
	s.w.WriteHeader(http.StatusOK)
}

func (s *sseWriter) emit(v any) error {
	s.start()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err // agente desconectou
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
