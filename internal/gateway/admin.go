package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/chwiee/nautilm/internal/auth"
	"github.com/chwiee/nautilm/internal/store"
	"github.com/chwiee/nautilm/internal/webauth"
)

// UseStore enables what needs the database: tokens created from the
// dashboard, sessions and audit.
func (g *Gateway) UseStore(s store.Store, tokens *auth.StoredTokens) {
	g.store, g.storedTokens = s, tokens
}

// UseEntra enables the Entra ID sign-in (needs UseStore).
func (g *Gateway) UseEntra(e *webauth.Entra) { g.entra = e }

func (g *Gateway) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/me", g.me)
	if g.entra != nil {
		mux.HandleFunc("GET /auth/login", g.entra.Login)
		mux.HandleFunc("GET /auth/callback", g.entra.Callback)
		mux.HandleFunc("POST /auth/logout", g.entra.Logout)
	}
	mux.HandleFunc("GET /v1/admin/tokens", g.adminOnly(g.listTokens))
	mux.HandleFunc("POST /v1/admin/tokens", g.adminOnly(g.createToken))
	mux.HandleFunc("POST /v1/admin/tokens/{id}/revoke", g.adminOnly(g.revokeToken))
	mux.HandleFunc("POST /v1/admin/tokens/{id}/rotate", g.adminOnly(g.rotateToken))
	mux.HandleFunc("GET /v1/admin/audit", g.adminOnly(g.listAudit))
	mux.HandleFunc("GET /v1/admin/aliases", g.adminOnly(g.listAliases))
}

// session returns the signed-in user (Entra), if any.
func (g *Gateway) session(r *http.Request) (store.Session, bool) {
	if g.entra == nil {
		return store.Session{}, false
	}
	return g.entra.SessionFrom(r)
}

func hasRole(s store.Session, role string) bool { return slices.Contains(s.Roles, role) }

// me tells the dashboard how to behave: sign-in button, token prompt, admin tab.
func (g *Gateway) me(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"entra": g.entra != nil, "usage_token": g.cfg.Dashboard.Token != "", "admin_api": g.store != nil}
	if s, ok := g.session(r); ok {
		out["user"] = s.User
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// adminOnly: signed-in admin; state-changing requests also need the
// dashboard's own Origin (CSRF protection for cookie-based sessions).
func (g *Gateway) adminOnly(h func(http.ResponseWriter, *http.Request, store.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.store == nil || g.entra == nil {
			writeError(w, http.StatusNotFound, "admin_disabled", "token management needs database and dashboard.entra in the nautilm config")
			return
		}
		s, ok := g.session(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in with Entra ID at /dashboard")
			return
		}
		if !hasRole(s, webauth.RoleAdmin) {
			g.metrics.Rejected(s.Email, "admin_forbidden")
			writeError(w, http.StatusForbidden, "forbidden", "only members of the nautilm admin groups manage tokens")
			return
		}
		if r.Method != http.MethodGet && !g.entra.SameOrigin(r) {
			g.metrics.Rejected(s.Email, "csrf")
			writeError(w, http.StatusForbidden, "csrf", "request must come from the nautilm dashboard (Origin check)")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r, s)
	}
}

func (g *Gateway) listTokens(w http.ResponseWriter, r *http.Request, _ store.Session) {
	list, err := g.store.ListTokens(r.Context())
	if err != nil {
		g.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": list, "now": g.now().UTC()})
}

type tokenRequest struct {
	Agent           string   `json:"agent"`
	Team            string   `json:"team"`
	CostCenter      string   `json:"cost_center"`
	Models          []string `json:"models"`
	TokensPerMinute int64    `json:"tokens_per_minute"`
	TokensPerDay    int64    `json:"tokens_per_day"`
	Note            string   `json:"note"`
	ValidDays       int      `json:"valid_days"`
}

// DefaultTokenDays: a token that never expires ends up forgotten somewhere.
const DefaultTokenDays = 90

func (g *Gateway) createToken(w http.ResponseWriter, r *http.Request, s store.Session) {
	var req tokenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "json", "invalid JSON: "+err.Error())
		return
	}
	if !agentName.MatchString(req.Agent) {
		writeError(w, http.StatusBadRequest, "invalid", "agent name: letters, numbers and . _ / : - (max 128)")
		return
	}
	if g.cfg.IsListedAgent(req.Agent) {
		writeError(w, http.StatusConflict, "agent_in_config",
			fmt.Sprintf("agent %q is defined in the nautilm config; pick another name (one place per agent)", req.Agent))
		return
	}
	for _, m := range req.Models {
		if _, ok := g.cfg.Models[m]; !ok && m != "*" {
			writeError(w, http.StatusBadRequest, "unknown_model", fmt.Sprintf("alias %q does not exist", m))
			return
		}
	}
	if req.ValidDays == 0 {
		req.ValidDays = DefaultTokenDays
	}
	clear, t, err := g.store.CreateToken(r.Context(), store.NewToken{Agent: req.Agent, Team: req.Team, CostCenter: req.CostCenter,
		Models: req.Models, TokensPerMinute: req.TokensPerMinute, TokensPerDay: req.TokensPerDay, Note: req.Note,
		TTL: time.Duration(req.ValidDays) * 24 * time.Hour}, s.Email)
	if errors.Is(err, store.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if err != nil {
		g.dbError(w, err)
		return
	}
	g.auditLog(r, s, "token.create", t.Agent, map[string]any{"id": t.ID, "prefix": t.Prefix, "models": t.Models,
		"team": t.Team, "cost_center": t.CostCenter, "expires_at": t.ExpiresAt})
	// The clear text goes out ONCE: it is not stored and cannot be read again.
	writeJSON(w, http.StatusCreated, map[string]any{"token": clear, "info": t})
}

func (g *Gateway) tokenID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid", "invalid token id")
		return 0, false
	}
	return id, true
}

func (g *Gateway) revokeToken(w http.ResponseWriter, r *http.Request, s store.Session) {
	id, ok := g.tokenID(w, r)
	if !ok {
		return
	}
	t, err := g.store.RevokeToken(r.Context(), id, s.Email)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "token not found")
		return
	}
	if err != nil {
		g.dbError(w, err)
		return
	}
	if g.storedTokens != nil {
		g.storedTokens.Forget() // effective now on this replica (others within the cache TTL)
	}
	g.auditLog(r, s, "token.revoke", t.Agent, map[string]any{"id": t.ID, "prefix": t.Prefix})
	writeJSON(w, http.StatusOK, map[string]any{"info": t})
}

// rotateToken creates a NEW token with the same profile; the old one keeps
// working until it is revoked, so the agent can switch without downtime.
func (g *Gateway) rotateToken(w http.ResponseWriter, r *http.Request, s store.Session) {
	id, ok := g.tokenID(w, r)
	if !ok {
		return
	}
	old, err := g.store.GetToken(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "token not found")
		return
	}
	if err != nil {
		g.dbError(w, err)
		return
	}
	if !old.Active(g.now()) {
		writeError(w, http.StatusConflict, "inactive", "this token is revoked or expired: create a new one instead")
		return
	}
	var req struct {
		ValidDays int `json:"valid_days"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req)
	if req.ValidDays == 0 {
		req.ValidDays = DefaultTokenDays
	}
	clear, t, err := g.store.CreateToken(r.Context(), store.NewToken{Agent: old.Agent, Team: old.Team, CostCenter: old.CostCenter,
		Models: old.Models, TokensPerMinute: old.TokensPerMinute, TokensPerDay: old.TokensPerDay,
		Note: "rotation of " + old.Prefix, TTL: time.Duration(req.ValidDays) * 24 * time.Hour}, s.Email)
	if err != nil {
		g.dbError(w, err)
		return
	}
	g.auditLog(r, s, "token.rotate", t.Agent, map[string]any{"old_id": old.ID, "new_id": t.ID, "new_prefix": t.Prefix})
	writeJSON(w, http.StatusCreated, map[string]any{"token": clear, "info": t, "replaces": old.ID})
}

func (g *Gateway) listAudit(w http.ResponseWriter, r *http.Request, _ store.Session) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	a, err := g.store.ListAudit(r.Context(), limit)
	if err != nil {
		g.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": a})
}

// listAliases feeds the "allowed models" field of the create form.
func (g *Gateway) listAliases(w http.ResponseWriter, _ *http.Request, _ store.Session) {
	type alias struct {
		Alias, Type, Provider, Model string
	}
	var out []alias
	for name, m := range g.cfg.Models {
		out = append(out, alias{name, m.Type, m.Provider, m.Model})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	writeJSON(w, http.StatusOK, map[string]any{"aliases": out, "default_valid_days": DefaultTokenDays})
}

func (g *Gateway) auditLog(r *http.Request, s store.Session, action, target string, details map[string]any) {
	g.log.Info("admin", "actor", s.Email, "action", action, "target", target, "details", details)
	if err := g.store.Audit(r.Context(), store.AuditEntry{Actor: s.Email, Action: action, Target: target, Details: details}); err != nil {
		g.log.Error("audit write failed", "error", err, "action", action, "actor", s.Email)
	}
}

func (g *Gateway) dbError(w http.ResponseWriter, err error) {
	g.log.Error("database error", "error", err)
	writeError(w, http.StatusServiceUnavailable, "database_unavailable", "the nautilm database is unavailable right now")
}
