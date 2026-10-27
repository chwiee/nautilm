package gateway

// The dashboard's Config tab (admins only). Every change:
//
//	builds the candidate config (file + saved changes + this change)
//	→ validates it with the same rules as the startup
//	→ tests it when it can call a model (provider, alias)
//	→ saves a NEW revision (append-only: history and rollback)
//	→ applies it here at once, on the other replicas within seconds
//	→ writes the audit log.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chwiee/nautilm/internal/live"
	"github.com/chwiee/nautilm/internal/pricing"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/internal/store"
	"github.com/chwiee/nautilm/pkg/openai"
)

// TestAgent is who pays for the test calls of the Config tab (ledger).
const TestAgent = "nautilm-config-test"

func (g *Gateway) configRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/config", g.configOnly(g.configView))
	mux.HandleFunc("POST /v1/admin/config/arn", g.configOnly(g.configARN))
	mux.HandleFunc("POST /v1/admin/config/providers", g.configOnly(g.configProviderAdd))
	mux.HandleFunc("POST /v1/admin/config/providers/{name}/delete", g.configOnly(g.configProviderDelete))
	mux.HandleFunc("POST /v1/admin/config/models", g.configOnly(g.configModelSave))
	mux.HandleFunc("POST /v1/admin/config/models/{alias}/delete", g.configOnly(g.configModelDelete))
	mux.HandleFunc("POST /v1/admin/config/pricing", g.configOnly(g.configPricing))
	mux.HandleFunc("POST /v1/admin/config/groups", g.configOnly(g.configGroups))
	mux.HandleFunc("GET /v1/admin/config/history", g.configOnly(g.configHistory))
	mux.HandleFunc("POST /v1/admin/config/rollback", g.configOnly(g.configRollback))
}

func (g *Gateway) configOnly(h func(http.ResponseWriter, *http.Request, store.Session)) http.HandlerFunc {
	return g.adminOnly(func(w http.ResponseWriter, r *http.Request, s store.Session) {
		if g.cfgMgr == nil {
			writeError(w, http.StatusNotFound, "config_disabled", "the Config tab needs database and dashboard.entra")
			return
		}
		h(w, r, s)
	})
}

func (g *Gateway) secretPrefix() string {
	if p := g.cfg.Dashboard.ProviderSecretPrefix; p != "" {
		return p
	}
	return live.DefaultSecretPrefix
}

// ---------- view ----------

type providerView struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Region    string   `json:"region,omitempty"`
	BaseURL   string   `json:"base_url,omitempty"`
	SecretARN string   `json:"secret_arn,omitempty"`
	Source    string   `json:"source"` // config | dashboard
	Broken    string   `json:"broken,omitempty"`
	Note      string   `json:"note,omitempty"`
	CreatedBy string   `json:"created_by,omitempty"`
	UsedBy    []string `json:"used_by"`
}

type modelView struct {
	Alias string `json:"alias"`
	live.ModelDef
	// Source: config (file), dashboard (added) or override (file alias
	// changed in the dashboard: deleting the override restores the file).
	Source string `json:"source"`
}

type priceView struct {
	live.PriceDef
	Source string `json:"source"`
}

type groupView struct {
	live.Group
	Source string `json:"source"`
}

func (g *Gateway) configView(w http.ResponseWriter, r *http.Request, s store.Session) {
	st := g.snap()
	ov := st.Overrides
	out := map[string]any{"status": g.cfgMgr.Status(), "secret_prefix": g.secretPrefix(), "revisions": ov.Revisions,
		"my_groups": s.Groups, "test_agent": TestAgent}

	providers := []providerView{}
	for _, name := range slices.Sorted(maps.Keys(st.Cfg.Providers)) {
		p := st.Cfg.Providers[name]
		v := providerView{Name: name, Type: p.Type, Region: p.Region, BaseURL: p.BaseURL, Source: "config", UsedBy: []string{}}
		if d, ok := ov.Providers[name]; ok {
			v.Source, v.SecretARN, v.Note, v.CreatedBy, v.BaseURL = "dashboard", d.SecretARN, d.Note, d.CreatedBy, d.BaseURL
			if v.BaseURL == "" && d.SecretARN != "" {
				v.BaseURL = "(from the secret)"
			}
			v.Broken = st.Broken[name]
		}
		for _, alias := range slices.Sorted(maps.Keys(st.Cfg.Models)) {
			if st.Cfg.Models[alias].Provider == name {
				v.UsedBy = append(v.UsedBy, alias)
			}
		}
		providers = append(providers, v)
	}
	out["providers"] = providers

	models := []modelView{}
	for _, alias := range slices.Sorted(maps.Keys(st.Cfg.Models)) {
		m := st.Cfg.Models[alias]
		src := "config"
		if _, ok := ov.Models[alias]; ok {
			src = "dashboard"
			if _, inFile := g.cfg.Models[alias]; inFile {
				src = "override"
			}
		}
		models = append(models, modelView{Alias: alias, Source: src, ModelDef: live.ModelDef{Type: m.Type, Provider: m.Provider,
			Model: m.Model, MaxTokens: m.MaxTokens, Dimensions: m.Dimensions, Fallback: m.Fallback, PromptCache: m.PromptCache}})
	}
	out["models"] = models

	prices := []priceView{}
	for _, p := range g.cfg.Pricing {
		prices = append(prices, priceView{Source: "config", PriceDef: live.PriceDef{Provider: p.Provider, Model: p.Model,
			Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite, Free: p.Free, EffectiveFrom: p.EffectiveFrom}})
	}
	for _, p := range ov.Pricing {
		prices = append(prices, priceView{Source: "dashboard", PriceDef: p})
	}
	out["pricing"] = prices
	out["missing_prices"] = st.Cfg.MissingPrices(g.now())
	out["strict_pricing"] = g.cfg.StrictPricing

	groups := map[string][]groupView{"admin": {}, "viewer": {}}
	if e := g.cfg.Dashboard.Entra; e != nil {
		for _, id := range e.AdminGroups {
			groups["admin"] = append(groups["admin"], groupView{Group: live.Group{ID: id}, Source: "config"})
		}
		for _, id := range e.ViewerGroups {
			groups["viewer"] = append(groups["viewer"], groupView{Group: live.Group{ID: id}, Source: "config"})
		}
	}
	for _, gr := range ov.Groups.Admin {
		groups["admin"] = append(groups["admin"], groupView{Group: gr, Source: "dashboard"})
	}
	for _, gr := range ov.Groups.Viewer {
		groups["viewer"] = append(groups["viewer"], groupView{Group: gr, Source: "dashboard"})
	}
	out["groups"] = groups
	writeJSON(w, http.StatusOK, out)
}

// ---------- saving ----------

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "json", "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// commit validates the candidate (whole merged config), saves the revision,
// applies it on this replica and audits. It answers the HTTP error itself.
func (g *Gateway) commit(w http.ResponseWriter, r *http.Request, s store.Session, kind string, payload any, parent int64,
	note string, rollbackOf int64, details map[string]any) (store.Revision, bool) {
	raw, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return store.Revision{}, false
	}
	if _, err := g.cfgMgr.Candidate(r.Context(), kind, raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_config", "not saved, the resulting config is invalid: "+err.Error())
		return store.Revision{}, false
	}
	rev, err := g.store.SaveRevision(r.Context(), store.NewRevision{Kind: kind, Payload: raw, Note: note, Parent: parent, RollbackOf: rollbackOf}, s.Email)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "another admin changed "+kind+" in the meantime: reload the Config tab and try again")
		return store.Revision{}, false
	case err != nil:
		g.dbError(w, err)
		return store.Revision{}, false
	}
	if err := g.cfgMgr.Reload(context.WithoutCancel(r.Context()), false); err != nil {
		g.log.Error("config saved but not applied on this replica", "revision", rev.ID, "error", err)
	}
	if details == nil {
		details = map[string]any{}
	}
	details["revision"] = rev.ID
	if rollbackOf > 0 {
		details["rollback_of"] = rollbackOf
	}
	g.auditLog(r, s, "config."+kind, note, details)
	return rev, true
}

// ---------- providers ----------

func (g *Gateway) configARN(w http.ResponseWriter, r *http.Request, _ store.Session) {
	var req struct {
		ARN string `json:"arn"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, live.InspectARN(req.ARN, g.secretPrefix()))
}

type providerRequest struct {
	live.TestInput
	Parent  int64 `json:"parent"`
	Replace bool  `json:"replace"` // edit an existing dashboard provider (same name)
}

// configProviderAdd runs the wizard test and, when every step passes, saves
// the provider (with no alias yet: it takes no traffic until one is added).
func (g *Gateway) configProviderAdd(w http.ResponseWriter, r *http.Request, s store.Session) {
	var req providerRequest
	if !readJSON(w, r, &req) {
		return
	}
	cur := g.snap().Overrides.Providers
	if _, exists := cur[req.Name]; exists && !req.Replace {
		writeError(w, http.StatusConflict, "exists", fmt.Sprintf("provider %q already exists; edit it instead", req.Name))
		return
	}
	res := g.cfgMgr.TestProvider(r.Context(), req.TestInput, g.secretPrefix())
	if res.Usage != nil {
		g.recordTest(r.Context(), s, "chat", served{alias: "(provider test)", provider: req.Name, model: req.TestModel}, res.Usage)
	}
	g.auditLog(r, s, "config.provider.test", req.Name, map[string]any{"ok": res.OK, "type": req.Def.Type,
		"secret_arn": req.Def.SecretARN, "steps": res.Steps})
	if !res.OK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"saved": false, "test": res})
		return
	}
	def := req.Def
	def.CreatedBy = s.Email
	next := maps.Clone(cur)
	if next == nil {
		next = map[string]live.ProviderDef{}
	}
	next[req.Name] = def
	rev, ok := g.commit(w, r, s, "providers", next, req.Parent, req.Name, 0,
		map[string]any{"provider": req.Name, "type": def.Type, "secret_arn": def.SecretARN, "replace": req.Replace})
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"saved": true, "test": res, "revision": rev})
}

func (g *Gateway) configProviderDelete(w http.ResponseWriter, r *http.Request, s store.Session) {
	var req struct {
		Parent int64 `json:"parent"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	st := g.snap()
	if _, ok := st.Overrides.Providers[name]; !ok {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("provider %q was not added in the dashboard (providers of the config file are read-only)", name))
		return
	}
	var users []string
	for alias, m := range st.Cfg.Models {
		if m.Provider == name {
			users = append(users, alias)
		}
	}
	if len(users) > 0 {
		slices.Sort(users)
		writeError(w, http.StatusConflict, "in_use", fmt.Sprintf("provider %q is used by %s: move or delete those aliases first", name, strings.Join(users, ", ")))
		return
	}
	next := maps.Clone(st.Overrides.Providers)
	delete(next, name)
	if rev, ok := g.commit(w, r, s, "providers", next, req.Parent, "delete "+name, 0, map[string]any{"deleted": name}); ok {
		writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
	}
}

// ---------- aliases ----------

type modelRequest struct {
	Alias  string        `json:"alias"`
	Def    live.ModelDef `json:"def"`
	Parent int64         `json:"parent"`
}

// configModelSave creates or changes an alias: validate, TEST with a real
// call, and only then save. A model that does not answer is never saved.
func (g *Gateway) configModelSave(w http.ResponseWriter, r *http.Request, s store.Session) {
	var req modelRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !agentName.MatchString(req.Alias) || strings.Contains(req.Alias, "/") {
		writeError(w, http.StatusBadRequest, "invalid", "alias: letters, numbers and . _ : - (max 128)")
		return
	}
	if req.Def.Type == "" {
		req.Def.Type = "chat"
	}
	next := maps.Clone(g.snap().Overrides.Models)
	if next == nil {
		next = map[string]live.ModelDef{}
	}
	prev, existed := next[req.Alias]
	next[req.Alias] = req.Def
	raw, _ := json.Marshal(next)
	cand, err := g.cfgMgr.Candidate(r.Context(), "models", raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_config", "not saved, the resulting config is invalid: "+err.Error())
		return
	}
	test := g.testAlias(r.Context(), cand, s, req.Alias)
	g.auditLog(r, s, "config.model.test", req.Alias, map[string]any{"ok": test.OK, "provider": req.Def.Provider,
		"model": req.Def.Model, "error": test.Error})
	if !test.OK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"saved": false, "test": test})
		return
	}
	details := map[string]any{"alias": req.Alias, "new": req.Def}
	if existed {
		details["old"] = prev
	} else if m, inFile := g.cfg.Models[req.Alias]; inFile {
		details["old"] = live.ModelDef{Type: m.Type, Provider: m.Provider, Model: m.Model, MaxTokens: m.MaxTokens, Fallback: m.Fallback}
	}
	if rev, ok := g.commit(w, r, s, "models", next, req.Parent, req.Alias, 0, details); ok {
		writeJSON(w, http.StatusCreated, map[string]any{"saved": true, "test": test, "revision": rev})
	}
}

func (g *Gateway) configModelDelete(w http.ResponseWriter, r *http.Request, s store.Session) {
	var req struct {
		Parent int64 `json:"parent"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	alias := r.PathValue("alias")
	cur := g.snap().Overrides.Models
	if _, ok := cur[alias]; !ok {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("alias %q has no dashboard change (aliases of the config file are read-only)", alias))
		return
	}
	next := maps.Clone(cur)
	delete(next, alias)
	note := "delete " + alias
	if _, inFile := g.cfg.Models[alias]; inFile {
		note = "restore " + alias + " from the config file"
	}
	if rev, ok := g.commit(w, r, s, "models", next, req.Parent, note, 0, map[string]any{"alias": alias, "old": cur[alias]}); ok {
		writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
	}
}

// AliasTest is the result of a real call made before saving an alias.
type AliasTest struct {
	OK        bool    `json:"ok"`
	Error     string  `json:"error,omitempty"`
	Code      string  `json:"code,omitempty"`
	LatencyMS int64   `json:"latency_ms"`
	Reply     string  `json:"reply,omitempty"`
	Dims      int     `json:"dims,omitempty"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	Priced    bool    `json:"priced"`
}

// testAlias calls the alias ITSELF (no fallback: we test this model) with a
// tiny request. The cost goes to the ledger as nautilm-config-test.
func (g *Gateway) testAlias(ctx context.Context, st *live.State, s store.Session, alias string) AliasTest {
	m := st.Cfg.Models[alias]
	p := st.Providers[m.Provider]
	used := served{alias: alias, provider: m.Provider, model: m.Model}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	t0 := time.Now()
	var res AliasTest
	var u *openai.Usage
	var err error
	if m.Type == "embedding" {
		emb, ok := p.(provider.Embedder)
		if !ok {
			return AliasTest{Error: fmt.Sprintf("provider %q does not generate embeddings", m.Provider)}
		}
		var vecs [][]float32
		vecs, u, err = emb.Embed(ctx, provider.EmbedCall{Model: m.Model, Input: []string{"nautilm config test"}, Dimensions: m.Dimensions})
		if err == nil && len(vecs) > 0 {
			res.Dims = len(vecs[0])
		}
	} else {
		max := 8
		req := &openai.ChatRequest{Model: alias, MaxTokens: &max, Messages: []openai.Message{{Role: "user", Content: openai.Text("Reply with the single word OK.")}}}
		// OpenAI-compatible providers forward the agent's original JSON (Raw):
		// build it from the request, exactly like a real call has it.
		var raw map[string]any
		b, _ := json.Marshal(req)
		json.Unmarshal(b, &raw)
		var resp *openai.ChatResponse
		resp, err = p.Chat(ctx, provider.Call{Model: m.Model, Alias: alias, DefaultMaxTokens: 8, Request: req, Raw: raw})
		if err == nil {
			u = resp.Usage
			if len(resp.Choices) > 0 && resp.Choices[0].Message.Content != nil {
				res.Reply = *resp.Choices[0].Message.Content
			}
		}
	}
	res.LatencyMS = time.Since(t0).Milliseconds()
	cost := g.recordTestWith(ctx, st, s, map[bool]string{true: "embedding", false: "chat"}[m.Type == "embedding"], used, u, err)
	if err != nil {
		perr := asProviderError(err)
		res.Error, res.Code = perr.Message, perr.Code
		return res
	}
	res.OK = true
	res.TokensIn, res.TokensOut = tokenCounts(u)
	res.CostUSD, res.Priced = cost.CostUSD, cost.Source != "unpriced"
	return res
}

func (g *Gateway) recordTest(ctx context.Context, s store.Session, kind string, used served, u *openai.Usage) {
	g.recordTestWith(ctx, g.snap(), s, kind, used, u, nil)
}

func (g *Gateway) recordTestWith(ctx context.Context, st *live.State, s store.Session, kind string, used served, u *openai.Usage, err error) pricing.Result {
	status := http.StatusOK
	if err != nil {
		status = asProviderError(err).Status
	}
	return g.record(ctx, st, billing{agent: TestAgent, caller: s.Email, team: "nautilm"}, kind, used, status, 0, u, "")
}

// ---------- pricing and groups ----------

func (g *Gateway) configPricing(w http.ResponseWriter, r *http.Request, s store.Session) {
	var req struct {
		Entries []live.PriceDef `json:"entries"`
		Parent  int64           `json:"parent"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Entries == nil {
		req.Entries = []live.PriceDef{}
	}
	for i, e := range req.Entries {
		if e.Provider == "" || e.Model == "" {
			writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("price %d: provider and model are required", i+1))
			return
		}
		if e.Input < 0 || e.Output < 0 || e.CacheRead < 0 || e.CacheWrite < 0 {
			writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("price %d: negative price", i+1))
			return
		}
	}
	if rev, ok := g.commit(w, r, s, "pricing", req.Entries, req.Parent, fmt.Sprintf("%d entries", len(req.Entries)), 0,
		map[string]any{"entries": req.Entries, "old": g.snap().Overrides.Pricing}); ok {
		writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
	}
}

func (g *Gateway) configGroups(w http.ResponseWriter, r *http.Request, s store.Session) {
	var req struct {
		live.Groups
		Parent int64 `json:"parent"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	clean := func(in []live.Group) []live.Group {
		out := []live.Group{}
		for _, gr := range in {
			gr.ID, gr.Name = strings.TrimSpace(gr.ID), strings.TrimSpace(gr.Name)
			if gr.ID != "" && !slices.ContainsFunc(out, func(x live.Group) bool { return x.ID == gr.ID }) {
				out = append(out, gr)
			}
		}
		return out
	}
	next := live.Groups{Admin: clean(req.Admin), Viewer: clean(req.Viewer)}
	if rev, ok := g.commit(w, r, s, "groups", next, req.Parent, fmt.Sprintf("%d admin, %d viewer", len(next.Admin), len(next.Viewer)), 0,
		map[string]any{"new": next, "old": g.snap().Overrides.Groups}); ok {
		writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
	}
}

// ---------- history and rollback ----------

func (g *Gateway) configHistory(w http.ResponseWriter, r *http.Request, _ store.Session) {
	kind := r.URL.Query().Get("kind")
	if !slices.Contains(store.RevisionKinds, kind) {
		writeError(w, http.StatusBadRequest, "invalid", "kind must be one of "+strings.Join(store.RevisionKinds, ", "))
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := g.store.ListRevisions(r.Context(), kind, limit)
	if err != nil {
		g.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "revisions": list, "current": g.snap().Overrides.Revisions[kind]})
}

// configRollback restores an old revision as a NEW one (history is never
// rewritten). It is validated like any change; it is not re-tested: it is a
// config that already worked, and the alias test would spend tokens again.
func (g *Gateway) configRollback(w http.ResponseWriter, r *http.Request, s store.Session) {
	var req struct {
		RevisionID int64 `json:"revision_id"`
		Parent     int64 `json:"parent"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	old, err := g.store.GetRevision(r.Context(), req.RevisionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "revision not found")
		return
	}
	if err != nil {
		g.dbError(w, err)
		return
	}
	if rev, ok := g.commit(w, r, s, old.Kind, old.Payload, req.Parent, fmt.Sprintf("rollback to revision %d", old.ID), old.ID, nil); ok {
		writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
	}
}
