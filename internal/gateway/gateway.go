// Package gateway é o coração do pedagio: recebe o pedido do agente no
// formato OpenAI, descobre quem é, resolve o apelido, chama o provider
// (com fallback) e responde — contando tokens no caminho.
//
// Fluxo de POST /v1/chat/completions:
//
//	autentica → lê o corpo → apelido existe? → agente pode usar? →
//	tenta apelido, depois fallbacks (só em 429/5xx) → responde (JSON ou SSE)
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/chwiee/pedagio/internal/auth"
	"github.com/chwiee/pedagio/internal/config"
	"github.com/chwiee/pedagio/internal/metrics"
	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

const maxBody = 10 << 20 // 10 MB: conversa longa com resultado de tool grande

type Gateway struct {
	cfg       *config.Config
	providers map[string]provider.Provider
	auth      auth.Authenticator
	metrics   *metrics.Registry
	log       *slog.Logger
}

func New(cfg *config.Config, providers map[string]provider.Provider, a auth.Authenticator, m *metrics.Registry, log *slog.Logger) *Gateway {
	return &Gateway{cfg: cfg, providers: providers, auth: a, metrics: m, log: log}
}

// Handler monta as rotas.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	mux.HandleFunc("GET /v1/models", g.models)
	mux.Handle("GET /metrics", g.metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	return mux
}

func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	id, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	list := openai.ModelList{Object: "list", Data: []openai.Model{}}
	for _, alias := range g.cfg.Allowed(id.Agent) {
		list.Data = append(list.Data, openai.Model{ID: alias, Object: "model", OwnedBy: "pedagio"})
	}
	writeJSON(w, http.StatusOK, list)
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id, ok := g.authenticate(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		g.reject(w, id, http.StatusRequestEntityTooLarge, "body", "corpo muito grande ou ilegível")
		return
	}
	var req openai.ChatRequest
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		g.reject(w, id, http.StatusBadRequest, "json", "JSON inválido: "+err.Error())
		return
	}
	_ = json.Unmarshal(raw, &rawMap)
	if len(req.Messages) == 0 {
		g.reject(w, id, http.StatusBadRequest, "messages", "messages vazio")
		return
	}

	// Agente pede APELIDO. ID cru de modelo é recusado de propósito: é isso
	// que permite trocar o modelo sem mexer em nenhum agente.
	allowed := g.cfg.Allowed(id.Agent)
	if _, exists := g.cfg.Models[req.Model]; !exists {
		g.reject(w, id, http.StatusNotFound, "unknown_model",
			fmt.Sprintf("modelo %q não existe no pedagio; use um apelido: %s", req.Model, strings.Join(allowed, ", ")))
		return
	}
	if !slices.Contains(allowed, req.Model) {
		g.reject(w, id, http.StatusForbidden, "forbidden_model",
			fmt.Sprintf("agente %q não pode usar %q; permitidos: %s", id.Agent, req.Model, strings.Join(allowed, ", ")))
		return
	}

	stream := req.Stream
	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	req.Stream = false

	resp, used, err := g.callChain(r.Context(), id, &req, rawMap, r.Header)
	trace := traceID(r.Header.Get("traceparent"))
	if err != nil {
		perr := asProviderError(err)
		g.log.Warn("chamada falhou", "agent", id.Agent, "alias", req.Model, "status", perr.Status,
			"code", perr.Code, "error", perr.Message, "trace_id", trace, "duration_ms", time.Since(start).Milliseconds())
		writeError(w, perr.Status, perr.Code, perr.Message)
		return
	}

	// Para o agente, o modelo é sempre o apelido que ele pediu. O modelo real
	// vai em headers (útil para debug e para o mirante).
	resp.Model = req.Model
	if resp.ID == "" {
		resp.ID = "chatcmpl-" + randomID()
	}
	w.Header().Set("X-Pedagio-Alias", used.alias)
	w.Header().Set("X-Pedagio-Provider", used.provider)
	w.Header().Set("X-Pedagio-Model", used.model)

	in, out, cached := usage(resp.Usage)
	g.log.Info("chat", "agent", id.Agent, "auth", id.Method, "alias", req.Model, "served_by", used.alias,
		"provider", used.provider, "model", used.model, "tokens_in", in, "tokens_out", out, "tokens_cached", cached,
		"tools", len(req.Tools), "stream", stream, "trace_id", trace, "duration_ms", time.Since(start).Milliseconds())

	if stream {
		writeSSE(w, resp, includeUsage)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type served struct{ alias, provider, model string }

// callChain tenta o apelido e depois os fallbacks. Só passa para o próximo
// em erro "retryable" (429/5xx): erro 4xx é culpa do pedido, e outro modelo
// daria o mesmo erro.
func (g *Gateway) callChain(ctx context.Context, id auth.Identity, req *openai.ChatRequest, raw map[string]any, h http.Header) (*openai.ChatResponse, served, error) {
	chain := g.cfg.Chain(req.Model)
	var lastErr error
	for i, alias := range chain {
		m := g.cfg.Models[alias]
		if i > 0 {
			g.metrics.Fallback(id.Agent, chain[i-1], alias)
			g.log.Warn("fallback", "agent", id.Agent, "from", chain[i-1], "to", alias, "error", lastErr)
		}
		p := g.providers[m.Provider]
		t0 := time.Now()
		resp, err := p.Chat(ctx, provider.Call{Model: m.Model, Request: req, Raw: raw, DefaultMaxTokens: m.MaxTokens, Header: h})
		status := http.StatusOK
		if err != nil {
			status = asProviderError(err).Status
		}
		var in, out, cached int
		if resp != nil {
			in, out, cached = usage(resp.Usage)
		}
		g.metrics.Call(id.Agent, alias, m.Model, status, time.Since(t0), in, out, cached)
		if err == nil {
			return resp, served{alias: alias, provider: m.Provider, model: m.Model}, nil
		}
		lastErr = err
		if !asProviderError(err).Retryable() || ctx.Err() != nil {
			break
		}
	}
	return nil, served{}, lastErr
}

func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	id, err := g.auth.Authenticate(r)
	if err == nil {
		return id, true
	}
	if errors.Is(err, auth.ErrNoCredentials) || errors.Is(err, auth.ErrInvalid) {
		g.metrics.Rejected("", "unauthenticated")
		writeError(w, http.StatusUnauthorized, "unauthenticated", "mande Authorization: Bearer <token da ServiceAccount ou token do pedagio>")
		return id, false
	}
	// Erro nosso (API server fora, por exemplo): 503, não 401.
	g.log.Error("autenticação indisponível", "error", err)
	writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "não foi possível validar a credencial agora")
	return id, false
}

func (g *Gateway) reject(w http.ResponseWriter, id auth.Identity, status int, reason, msg string) {
	g.metrics.Rejected(id.Agent, reason)
	g.log.Info("rejeitado", "agent", id.Agent, "status", status, "reason", reason, "message", msg)
	writeError(w, status, reason, msg)
}

// writeSSE entrega a resposta pronta como stream. Ainda é "stream simulado":
// o pedagio espera a resposta inteira e manda em poucos eventos. Funciona
// com qualquer cliente que exige stream; o tempo até o 1º token só melhora
// quando o ConverseStream entrar (ver docs/ROADMAP.md).
func writeSSE(w http.ResponseWriter, resp *openai.ChatResponse, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	send := func(c openai.ChatChunk) {
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	base := openai.ChatChunk{ID: resp.ID, Object: "chat.completion.chunk", Created: resp.Created, Model: resp.Model}

	for _, ch := range resp.Choices {
		c := base
		d := openai.Delta{Role: "assistant"}
		if ch.Message.Content != nil {
			d.Content = *ch.Message.Content
		}
		for i, tc := range ch.Message.ToolCalls {
			idx := i
			tc.Index = &idx
			d.ToolCalls = append(d.ToolCalls, tc)
		}
		c.Choices = []openai.ChunkChoice{{Index: ch.Index, Delta: d}}
		send(c)

		fin := base
		reason := ch.FinishReason
		fin.Choices = []openai.ChunkChoice{{Index: ch.Index, Delta: openai.Delta{}, FinishReason: &reason}}
		send(fin)
	}
	if includeUsage && resp.Usage != nil {
		u := base
		u.Choices = []openai.ChunkChoice{}
		u.Usage = resp.Usage
		send(u)
	}
	io.WriteString(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "api_error"
	}
	writeJSON(w, status, openai.ErrorResponse{Error: openai.ErrorBody{Message: msg, Type: typ, Code: code}})
}

func asProviderError(err error) *provider.Error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, context.Canceled) {
		return &provider.Error{Status: 499, Code: "client_closed", Message: "cliente desistiu"}
	}
	return &provider.Error{Status: http.StatusBadGateway, Code: "upstream_error", Message: err.Error()}
}

func usage(u *openai.Usage) (in, out, cached int) {
	if u == nil {
		return 0, 0, 0
	}
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return u.PromptTokens, u.CompletionTokens, cached
}

// traceID extrai o trace-id do header W3C traceparent (00-<trace>-<span>-<flags>).
func traceID(tp string) string {
	parts := strings.Split(tp, "-")
	if len(parts) == 4 && len(parts[1]) == 32 {
		return parts[1]
	}
	return ""
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
