package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/chwiee/pedagio/internal/provider"
	"github.com/chwiee/pedagio/pkg/openai"
)

// maxEmbedInputs limita o lote: protege o pedagio e a cota do Bedrock.
const maxEmbedInputs = 256

// embeddings atende POST /v1/embeddings. Mesmas regras do chat (auth,
// apelido, permissão), com uma diferença de propósito: SEM fallback.
// Vetores de modelos diferentes vivem em espaços diferentes; cair para
// outro modelo produziria vetores que não se comparam com o índice.
func (g *Gateway) embeddings(w http.ResponseWriter, r *http.Request) {
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
	var req openai.EmbeddingRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		g.reject(w, id, http.StatusBadRequest, "json", "JSON inválido: "+err.Error())
		return
	}
	if len(req.Input) == 0 || len(req.Input) > maxEmbedInputs {
		g.reject(w, id, http.StatusBadRequest, "input", fmt.Sprintf("input precisa ter de 1 a %d textos", maxEmbedInputs))
		return
	}
	g.reportMasked(w, id, g.maskEmbed(req.Input))

	m, exists := g.cfg.Models[req.Model]
	allowed := g.cfg.Allowed(id.Agent)
	switch {
	case !exists:
		g.reject(w, id, http.StatusNotFound, "unknown_model", fmt.Sprintf("modelo %q não existe no pedagio; use um apelido", req.Model))
		return
	case m.Type != "embedding":
		g.reject(w, id, http.StatusBadRequest, "wrong_model_type", fmt.Sprintf("%q é um modelo de chat, não de embedding", req.Model))
		return
	case !slices.Contains(allowed, req.Model):
		g.reject(w, id, http.StatusForbidden, "forbidden_model",
			fmt.Sprintf("agente %q não pode usar %q; permitidos: %s", id.Agent, req.Model, strings.Join(allowed, ", ")))
		return
	}
	lim := g.limits(id)
	if !g.checkQuota(w, r, id, lim) {
		return
	}
	emb, ok := g.providers[m.Provider].(provider.Embedder)
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", fmt.Sprintf("provider %q não gera embeddings", m.Provider))
		return
	}

	dims := m.Dimensions
	if req.Dimensions != nil {
		dims = *req.Dimensions
	}
	vecs, u, err := emb.Embed(r.Context(), provider.EmbedCall{Model: m.Model, Input: req.Input, Dimensions: dims, Header: r.Header})
	status := http.StatusOK
	if err != nil {
		status = asProviderError(err).Status
	}
	in, _, _ := usage(u)
	g.spendQuota(r.Context(), id, lim, in)
	g.metrics.Call(id.Agent, req.Model, m.Model, status, time.Since(start), in, 0, 0)
	if err != nil {
		perr := asProviderError(err)
		g.log.Warn("embeddings falhou", "agent", id.Agent, "alias", req.Model, "status", perr.Status, "error", perr.Message)
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
	setServedHeaders(w, served{alias: req.Model, provider: m.Provider, model: m.Model})
	g.log.Info("embeddings", "agent", id.Agent, "alias", req.Model, "provider", m.Provider, "model", m.Model,
		"inputs", len(req.Input), "dims", dim, "tokens_in", in, "trace_id", traceID(r.Header.Get("traceparent")),
		"duration_ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, resp)
}
