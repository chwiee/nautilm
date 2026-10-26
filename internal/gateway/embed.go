package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/pkg/openai"
)

// maxEmbedInputs caps the batch: protects nautilm and the Bedrock quota.
const maxEmbedInputs = 256

// embeddings serves POST /v1/embeddings. Same rules as chat (auth, alias,
// permission, masking, quota, billing), with one difference on purpose: NO
// fallback. Vectors from different models live in different spaces; falling
// back to another model would build vectors that do not compare with the index.
func (g *Gateway) embeddings(w http.ResponseWriter, r *http.Request) {
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
	var req openai.EmbeddingRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		g.reject(w, id, http.StatusBadRequest, "json", "invalid JSON: "+err.Error())
		return
	}
	if len(req.Input) == 0 || len(req.Input) > maxEmbedInputs {
		g.reject(w, id, http.StatusBadRequest, "input", fmt.Sprintf("input must have 1 to %d texts", maxEmbedInputs))
		return
	}
	g.reportMasked(w, id, g.maskEmbed(req.Input))

	m, exists := g.cfg.Models[req.Model]
	allowed := g.allowed(id)
	switch {
	case !exists:
		g.reject(w, id, http.StatusNotFound, "unknown_model", fmt.Sprintf("model %q does not exist in nautilm; use an alias", req.Model))
		return
	case m.Type != "embedding":
		g.reject(w, id, http.StatusBadRequest, "wrong_model_type", fmt.Sprintf("%q is a chat model, not an embedding model", req.Model))
		return
	case !slices.Contains(allowed, req.Model):
		g.reject(w, id, http.StatusForbidden, "forbidden_model",
			fmt.Sprintf("agent %q may not use %q; allowed: %s", id.Agent, req.Model, strings.Join(allowed, ", ")))
		return
	}
	lim := g.limits(id)
	if !g.checkQuota(w, r, id, lim) {
		return
	}
	emb, ok := g.providers[m.Provider].(provider.Embedder)
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", fmt.Sprintf("provider %q does not generate embeddings", m.Provider))
		return
	}
	bill := g.billingFor(r, id)
	w.Header().Set("X-Nautilm-Billed-To", bill.agent)
	trace := traceID(r.Header.Get("traceparent"))

	dims := m.Dimensions
	if req.Dimensions != nil {
		dims = *req.Dimensions
	}
	used := served{alias: req.Model, provider: m.Provider, model: m.Model}
	vecs, u, err := emb.Embed(r.Context(), provider.EmbedCall{Model: m.Model, Input: req.Input, Dimensions: dims, Header: r.Header})
	status := http.StatusOK
	if err != nil {
		status = asProviderError(err).Status
	}
	cost := g.record(r.Context(), bill, "embedding", used, status, time.Since(start), u, trace)
	in, _ := tokenCounts(u)
	g.spendQuota(r.Context(), id, lim, in)
	if err != nil {
		perr := asProviderError(err)
		g.log.Warn("embeddings failed", "agent", id.Agent, "alias", req.Model, "status", perr.Status, "error", perr.Message)
		writeError(w, perr.Status, perr.Code, perr.Message)
		return
	}

	resp := openai.EmbeddingResponse{Object: "list", Model: req.Model, Usage: u}
	for i, v := range vecs {
		resp.Data = append(resp.Data, openai.EmbeddingData{Object: "embedding", Index: i, Embedding: v})
	}
	dim := 0
	if len(vecs) > 0 {
		dim = len(vecs[0])
	}
	setServedHeaders(w, used)
	setCostHeaders(w, cost)
	g.log.Info("embeddings", "agent", id.Agent, "billed_to", bill.agent, "alias", req.Model, "provider", m.Provider,
		"model", m.Model, "inputs", len(req.Input), "dims", dim, "tokens_in", in, "cost_usd", cost.CostUSD,
		"price_source", cost.Source, "trace_id", trace, "duration_ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, resp)
}
