// Package gateway is the heart of nautilm: it takes the agent's request in
// the OpenAI format, finds out who it is, resolves the alias, calls the
// provider (with fallback) and answers, accounting tokens and cost on the way.
//
// POST /v1/chat/completions flow:
//
//	authenticate → read body → mask sensitive data → alias exists? →
//	agent allowed? → quota left? → try alias, then fallbacks (429/5xx only)
//	→ record usage and cost of every attempt → answer (JSON or SSE)
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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chwiee/nautilm/internal/auth"
	"github.com/chwiee/nautilm/internal/config"
	"github.com/chwiee/nautilm/internal/live"
	"github.com/chwiee/nautilm/internal/mask"
	"github.com/chwiee/nautilm/internal/metrics"
	"github.com/chwiee/nautilm/internal/pricing"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/internal/quota"
	"github.com/chwiee/nautilm/internal/store"
	"github.com/chwiee/nautilm/internal/usage"
	"github.com/chwiee/nautilm/internal/webauth"
	"github.com/chwiee/nautilm/pkg/openai"
)

const maxBody = 10 << 20 // 10 MB: a long conversation with a big tool result

// HeaderOnBehalfOf lets an allowed caller (the orchestrator) bill a call to
// the agent it is serving. See config.Agent.OnBehalfOf.
const HeaderOnBehalfOf = "X-Nautilm-On-Behalf-Of"

type Gateway struct {
	// cfg is the config FILE (base). Aliases, providers and prices in effect
	// come from snap(): the file plus the Config tab changes.
	cfg     *config.Config
	static  *live.State
	cfgMgr  *live.Manager
	auth    auth.Authenticator
	metrics *metrics.Registry
	log     *slog.Logger
	masker  *mask.Masker
	quota   *quota.Quota
	usage   usage.Store
	now     func() time.Time

	store        store.Store
	storedTokens *auth.StoredTokens
	entra        *webauth.Entra
}

func New(cfg *config.Config, providers map[string]provider.Provider, a auth.Authenticator, m *metrics.Registry, log *slog.Logger) *Gateway {
	// The config was validated (Validate calls mask.New), so this cannot fail.
	mk, _ := mask.New(cfg.Masking)
	g := &Gateway{cfg: cfg, static: live.Static(cfg, providers), auth: a, metrics: m, log: log, masker: mk, now: time.Now}
	if !cfg.Quotas.Disabled {
		// Memory by default; main switches to Valkey when valkey_url is set.
		g.quota = quota.New(quota.NewMemory())
	}
	if !cfg.Usage.Disabled {
		g.usage = usage.NewMemory()
	}
	return g
}

// UseConfigManager makes the Config tab changes take effect (needs UseStore).
func (g *Gateway) UseConfigManager(m *live.Manager) { g.cfgMgr = m }

// snap returns the config in effect. A request takes ONE snapshot and uses
// it to the end, so a change saved meanwhile never mixes two configs.
func (g *Gateway) snap() *live.State {
	if g.cfgMgr != nil {
		if s := g.cfgMgr.Current(); s != nil {
			return s
		}
	}
	return g.static
}

// UseQuotaCounter swaps the quota counter (e.g. Valkey instead of memory).
func (g *Gateway) UseQuotaCounter(c quota.Counter) {
	if g.quota != nil {
		g.quota.Counter = c
	}
}

// UseUsageStore swaps the usage ledger store (e.g. Valkey instead of memory).
func (g *Gateway) UseUsageStore(s usage.Store) {
	if g.usage != nil {
		g.usage = s
	}
}

// Handler builds the routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	mux.HandleFunc("POST /v1/embeddings", g.embeddings)
	mux.HandleFunc("GET /v1/models", g.models)
	mux.HandleFunc("GET /v1/usage", g.usageJSON)
	mux.HandleFunc("GET /v1/usage.csv", g.usageCSV)
	mux.HandleFunc("GET /dashboard", g.dashboard)
	g.authRoutes(mux)
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
	for _, alias := range g.allowed(g.snap(), id) {
		list.Data = append(list.Data, openai.Model{ID: alias, Object: "model", OwnedBy: "nautilm"})
	}
	writeJSON(w, http.StatusOK, list)
}

// billing says who pays for a request and how to file it.
type billing struct {
	agent      string // who pays
	caller     string // who called (authenticated)
	team       string
	costCenter string
}

var agentName = regexp.MustCompile(`^[A-Za-z0-9._/:-]{1,128}$`)

// billingFor resolves who pays. The caller pays, unless it is allowed to act
// on behalf of others (on_behalf_of: true) and names the agent it serves.
// A header from a caller that is not allowed is IGNORED (logged): otherwise
// any agent could push its bill to another one.
func (g *Gateway) billingFor(r *http.Request, id auth.Identity) billing {
	b := billing{agent: id.Agent, caller: id.Agent}
	if obo := strings.TrimSpace(r.Header.Get(HeaderOnBehalfOf)); obo != "" && obo != id.Agent {
		switch {
		case !g.canActOnBehalf(id):
			g.metrics.Rejected(id.Agent, "on_behalf_of_not_allowed")
			g.log.Warn("on-behalf-of ignored: caller is not allowed", "caller", id.Agent, "on_behalf_of", obo)
		case !agentName.MatchString(obo):
			g.log.Warn("on-behalf-of ignored: invalid agent name", "caller", id.Agent, "on_behalf_of", obo)
		default:
			b.agent = obo
		}
	}
	b.team, b.costCenter = g.billingOf(id, b.agent)
	return b
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id, ok := g.authenticate(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		g.reject(w, id, http.StatusRequestEntityTooLarge, "body", "body too large or unreadable")
		return
	}
	var req openai.ChatRequest
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		g.reject(w, id, http.StatusBadRequest, "json", "invalid JSON: "+err.Error())
		return
	}
	_ = json.Unmarshal(raw, &rawMap)
	// Mask BEFORE anything else: from here on (provider, logs, fallback)
	// nobody sees the sensitive data again.
	masked, err := g.maskChat(rawMap, &req)
	if err != nil {
		g.reject(w, id, http.StatusBadRequest, "mask", err.Error())
		return
	}
	g.reportMasked(w, id, masked)
	if len(req.Messages) == 0 {
		g.reject(w, id, http.StatusBadRequest, "messages", "messages is empty")
		return
	}

	// Agents ask for an ALIAS. A raw model ID is refused on purpose: that is
	// what makes switching models possible without touching any agent.
	st := g.snap()
	allowed := g.allowed(st, id)
	m, exists := st.Cfg.Models[req.Model]
	if !exists {
		g.reject(w, id, http.StatusNotFound, "unknown_model",
			fmt.Sprintf("model %q does not exist in nautilm; use an alias: %s", req.Model, strings.Join(allowed, ", ")))
		return
	}
	if m.Type == "embedding" {
		g.reject(w, id, http.StatusBadRequest, "wrong_model_type", fmt.Sprintf("%q is an embedding model; use /v1/embeddings", req.Model))
		return
	}
	if !slices.Contains(allowed, req.Model) {
		g.reject(w, id, http.StatusForbidden, "forbidden_model",
			fmt.Sprintf("agent %q may not use %q; allowed: %s", id.Agent, req.Model, strings.Join(allowed, ", ")))
		return
	}
	lim := g.limits(id)
	if !g.checkQuota(w, r, id, lim) {
		return
	}
	bill := g.billingFor(r, id)
	w.Header().Set("X-Nautilm-Billed-To", bill.agent)

	stream := req.Stream
	req.Stream = false // providers always get false; the mode goes through the interface
	callID := "chatcmpl-" + randomID()
	trace := traceID(r.Header.Get("traceparent"))

	if stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		res := g.streamChain(r.Context(), st, w, id, bill, &req, rawMap, r.Header, callID, includeUsage, trace)
		in, out := tokenCounts(res.usage)
		g.spendQuota(r.Context(), id, lim, in+out)
		attrs := []any{"agent", id.Agent, "billed_to", bill.agent, "auth", id.Method, "alias", req.Model,
			"served_by", res.used.alias, "provider", res.used.provider, "model", res.used.model,
			"tokens_in", in, "tokens_out", out, "cost_usd", res.cost.CostUSD, "price_source", res.cost.Source,
			"tools", len(req.Tools), "stream", true, "stream_mode", res.mode,
			"trace_id", trace, "ttft_ms", res.ttft.Milliseconds(), "duration_ms", time.Since(start).Milliseconds()}
		if res.err != nil {
			perr := asProviderError(res.err)
			g.log.Warn("stream failed", append(attrs, "status", perr.Status, "code", perr.Code, "error", perr.Message, "mid_stream", res.started)...)
			if !res.started {
				// Nothing was sent yet: a normal HTTP error is still possible.
				writeError(w, perr.Status, perr.Code, perr.Message)
			}
			return
		}
		g.log.Info("chat", attrs...)
		return
	}

	resp, used, cost, err := g.callChain(r.Context(), st, bill, &req, rawMap, r.Header, callID, trace)
	if err != nil {
		perr := asProviderError(err)
		g.log.Warn("call failed", "agent", id.Agent, "billed_to", bill.agent, "alias", req.Model, "status", perr.Status,
			"code", perr.Code, "error", perr.Message, "trace_id", trace, "duration_ms", time.Since(start).Milliseconds())
		writeError(w, perr.Status, perr.Code, perr.Message)
		return
	}

	// To the agent, the model is always the alias it asked for. The real
	// model and the cost go in headers (debug, Mirante).
	resp.Model = req.Model
	resp.ID = callID
	setServedHeaders(w, used)
	setCostHeaders(w, cost)

	in, out := tokenCounts(resp.Usage)
	g.spendQuota(r.Context(), id, lim, in+out)
	g.log.Info("chat", "agent", id.Agent, "billed_to", bill.agent, "auth", id.Method, "alias", req.Model,
		"served_by", used.alias, "provider", used.provider, "model", used.model, "tokens_in", in, "tokens_out", out,
		"tokens_cache_read", resp.Usage.CacheRead(), "tokens_cache_write", resp.Usage.CacheWrite(),
		"cost_usd", cost.CostUSD, "price_source", cost.Source, "price_version", cost.Version,
		"tools", len(req.Tools), "stream", false, "trace_id", trace, "duration_ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, resp)
}

type served struct{ alias, provider, model string }

func setServedHeaders(w http.ResponseWriter, s served) {
	w.Header().Set("X-Nautilm-Alias", s.alias)
	w.Header().Set("X-Nautilm-Provider", s.provider)
	w.Header().Set("X-Nautilm-Model", s.model)
}

func setCostHeaders(w http.ResponseWriter, c pricing.Result) {
	w.Header().Set("X-Nautilm-Price-Source", c.Source)
	if c.Source != pricing.SourceUnpriced {
		w.Header().Set("X-Nautilm-Cost-USD", strconv.FormatFloat(c.CostUSD, 'f', 8, 64))
	}
}

// callChain tries the alias and then the fallbacks. It only moves on after
// a "retryable" error (429/5xx): a 4xx is the request's fault, and another
// model would give the same error. Every attempt is recorded (usage, cost).
func (g *Gateway) callChain(ctx context.Context, st *live.State, b billing, req *openai.ChatRequest, raw map[string]any, h http.Header, callID, trace string) (*openai.ChatResponse, served, pricing.Result, error) {
	chain := st.Cfg.Chain(req.Model)
	var lastErr error
	for i, alias := range chain {
		m := st.Cfg.Models[alias]
		if i > 0 {
			g.metrics.Fallback(b.agent, chain[i-1], alias)
			g.log.Warn("fallback", "agent", b.caller, "from", chain[i-1], "to", alias, "error", lastErr)
		}
		used := served{alias: alias, provider: m.Provider, model: m.Model}
		t0 := time.Now()
		resp, err := st.Providers[m.Provider].Chat(ctx, provider.Call{Model: m.Model, Request: req, Raw: raw,
			DefaultMaxTokens: m.MaxTokens, Header: h, ID: callID, Alias: req.Model})
		status := http.StatusOK
		var u *openai.Usage
		if err != nil {
			status = asProviderError(err).Status
		} else {
			u = resp.Usage
		}
		cost := g.record(ctx, st, b, "chat", used, status, time.Since(t0), u, trace)
		if err == nil {
			return resp, used, cost, nil
		}
		lastErr = err
		if !asProviderError(err).Retryable() || ctx.Err() != nil {
			break
		}
	}
	return nil, served{}, pricing.Result{}, lastErr
}

// record accounts ONE attempt: prices it by the provider+model that served
// it (on today's price), updates the metrics and writes the usage ledger.
// If the ledger write fails (Valkey down), the full record goes to the log
// so it can be recovered, and nautilm_usage_lost_total is incremented.
func (g *Gateway) record(ctx context.Context, st *live.State, b billing, kind string, used served, status int, d time.Duration, u *openai.Usage, trace string) pricing.Result {
	t := tokensOf(u)
	var reported *float64
	if u != nil {
		reported = u.Cost
	}
	now := g.now()
	cost := st.Cfg.Catalog().Cost(used.provider, used.model, now, t, reported)
	g.metrics.Call(b.agent, used.alias, used.provider, used.model, status, d, t.Input, t.Output, t.CacheRead, t.CacheWrite)
	g.metrics.Cost(b.agent, used.provider, used.model, cost.Source, cost.CostUSD)
	if cost.Source == pricing.SourceUnpriced && t.Input+t.Output > 0 {
		g.metrics.Unpriced(b.agent, used.provider, used.model)
	}
	if g.usage == nil {
		return cost
	}
	rec := usage.Record{Time: now, Agent: b.agent, Caller: b.caller, Team: b.team, CostCenter: b.costCenter,
		Kind: kind, Alias: used.alias, Provider: used.provider, Model: used.model, Status: status,
		Tokens: t, Cost: cost, TraceID: trace, Duration: d}
	// The call already happened: the record must not die with the request.
	if err := g.usage.Add(context.WithoutCancel(ctx), rec); err != nil {
		g.metrics.UsageLost()
		g.log.Error("usage record lost", "error", err, "time", now.UTC().Format(time.RFC3339), "agent", b.agent,
			"caller", b.caller, "team", b.team, "cost_center", b.costCenter, "kind", kind, "alias", used.alias,
			"provider", used.provider, "model", used.model, "status", status, "tokens_in", t.Input, "tokens_out", t.Output,
			"tokens_cache_read", t.CacheRead, "tokens_cache_write", t.CacheWrite, "cost_usd", cost.CostUSD,
			"price_source", cost.Source, "price_version", cost.Version, "trace_id", trace)
	}
	return cost
}

func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	id, err := g.auth.Authenticate(r)
	if err == nil {
		return id, true
	}
	if errors.Is(err, auth.ErrNoCredentials) || errors.Is(err, auth.ErrInvalid) {
		g.metrics.Rejected("", "unauthenticated")
		writeError(w, http.StatusUnauthorized, "unauthenticated", "send Authorization: Bearer <ServiceAccount token or nautilm token>")
		return id, false
	}
	// Our own error (API server down, for example): 503, not 401.
	g.log.Error("authentication unavailable", "error", err)
	writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "could not validate the credentials right now")
	return id, false
}

func (g *Gateway) reject(w http.ResponseWriter, id auth.Identity, status int, reason, msg string) {
	g.metrics.Rejected(id.Agent, reason)
	g.log.Info("rejected", "agent", id.Agent, "status", status, "reason", reason, "message", msg)
	writeError(w, status, reason, msg)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	typ := "invalid_request_error"
	switch {
	case status == http.StatusTooManyRequests:
		// OpenAI clients use this type to back off. The code tells WHICH
		// limit: quota_minute / quota_day (nautilm) or provider_quota_day
		// and ThrottlingException (the provider account).
		typ = "rate_limit_error"
	case status >= 500:
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
		return &provider.Error{Status: 499, Code: "client_closed", Message: "client gave up"}
	}
	return &provider.Error{Status: http.StatusBadGateway, Code: "upstream_error", Message: err.Error()}
}

// tokensOf normalizes usage into pricing tokens (see openai.Usage).
func tokensOf(u *openai.Usage) pricing.Tokens {
	if u == nil {
		return pricing.Tokens{}
	}
	return pricing.Tokens{Input: u.PromptTokens, Output: u.CompletionTokens, CacheRead: u.CacheRead(), CacheWrite: u.CacheWrite()}
}

func tokenCounts(u *openai.Usage) (in, out int) {
	t := tokensOf(u)
	return t.Input, t.Output
}

// traceID extracts the trace-id from the W3C traceparent header (00-<trace>-<span>-<flags>).
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
