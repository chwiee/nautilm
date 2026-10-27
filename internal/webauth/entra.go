// Package webauth signs people in to the nautilm dashboard with Microsoft
// Entra ID (OpenID Connect) and maps Entra groups to roles.
//
// The whole flow runs on the server (Authorization Code + PKCE, state and
// nonce): the Entra tokens are validated here (signature, issuer, audience,
// tenant, nonce) and never reach the browser. The browser only gets an
// HttpOnly session cookie; the session lives in the database, so signing
// out really ends it.
//
// Roles:
//
//	admin  = member of one of admin_groups  (manage tokens, see everything)
//	viewer = member of one of viewer_groups (see usage and cost)
//	an admin is also a viewer; anyone else is denied.
package webauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/chwiee/nautilm/internal/store"
)

const (
	SessionCookie = "nautilm_session"
	flowCookie    = "nautilm_oidc"
	RoleAdmin     = "admin"
	RoleViewer    = "viewer"
)

type Config struct {
	Issuer       string
	TenantID     string
	ClientID     string
	ClientSecret string
	PublicURL    string // https://nautilm.corp.example (redirect = <public>/auth/callback)
	AdminGroups  []string
	ViewerGroups []string
	SessionTTL   time.Duration
	// CheckTenant rejects tokens whose "tid" is not TenantID (default true;
	// the local mock sets the same tid, so it stays on there too).
	CheckTenant bool
	// Groups returns the group map IN EFFECT (config file + Config tab).
	// Nil = AdminGroups/ViewerGroups above.
	Groups func() (admin, viewer []string)
}

func (e *Entra) groups() (admin, viewer []string) {
	if e.cfg.Groups != nil {
		return e.cfg.Groups()
	}
	return e.cfg.AdminGroups, e.cfg.ViewerGroups
}

type Entra struct {
	cfg   Config
	store store.Store
	log   *slog.Logger
	Now   func() time.Time

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func New(cfg Config, s store.Store, log *slog.Logger) *Entra {
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	return &Entra{cfg: cfg, store: s, log: log, Now: time.Now}
}

// setup discovers the provider on first use (and retries on the next
// request if Entra was unreachable), instead of failing the startup.
func (e *Entra) setup(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.oauth != nil {
		return e.oauth, e.verifier, nil
	}
	p, err := oidc.NewProvider(ctx, e.cfg.Issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("discovering %s: %w", e.cfg.Issuer, err)
	}
	e.oauth = &oauth2.Config{
		ClientID: e.cfg.ClientID, ClientSecret: e.cfg.ClientSecret, Endpoint: p.Endpoint(),
		RedirectURL: e.cfg.PublicURL + "/auth/callback",
		Scopes:      []string{oidc.ScopeOpenID, "profile", "email"},
	}
	e.verifier = p.Verifier(&oidc.Config{ClientID: e.cfg.ClientID})
	return e.oauth, e.verifier, nil
}

func (e *Entra) secure() bool { return strings.HasPrefix(e.cfg.PublicURL, "https://") }

func random() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Login sends the browser to Entra.
func (e *Entra) Login(w http.ResponseWriter, r *http.Request) {
	oc, _, err := e.setup(r.Context())
	if err != nil {
		e.log.Error("entra unavailable", "error", err)
		page(w, http.StatusServiceUnavailable, "Sign-in unavailable", "nautilm could not reach Microsoft Entra ID. Try again in a moment.")
		return
	}
	state, nonce, verifier := random(), random(), oauth2.GenerateVerifier()
	// Lax: the cookie must come back on the top-level redirect from Entra.
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: state + "." + nonce + "." + verifier, Path: "/auth/",
		MaxAge: 600, HttpOnly: true, Secure: e.secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, oc.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

type entraClaims struct {
	OID               string            `json:"oid"`
	TID               string            `json:"tid"`
	Name              string            `json:"name"`
	PreferredUsername string            `json:"preferred_username"`
	Email             string            `json:"email"`
	Groups            []string          `json:"groups"`
	ClaimNames        map[string]string `json:"_claim_names"`
}

// Callback validates Entra's answer and opens a session.
func (e *Entra) Callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if msg := q.Get("error"); msg != "" {
		e.denied(ctx, w, "", "entra returned an error", q.Get("error_description"))
		return
	}
	c, err := r.Cookie(flowCookie)
	parts := []string{}
	if err == nil {
		parts = strings.Split(c.Value, ".")
	}
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: e.secure()})
	if len(parts) != 3 || q.Get("state") == "" || q.Get("state") != parts[0] {
		e.denied(ctx, w, "", "invalid sign-in state", "start again from the dashboard (the sign-in link expires in 10 minutes)")
		return
	}
	oc, verifier, err := e.setup(ctx)
	if err != nil {
		page(w, http.StatusServiceUnavailable, "Sign-in unavailable", "nautilm could not reach Microsoft Entra ID.")
		return
	}
	tok, err := oc.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(parts[2]))
	if err != nil {
		e.denied(ctx, w, "", "code exchange failed", err.Error())
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := verifier.Verify(ctx, raw)
	if err != nil {
		e.denied(ctx, w, "", "invalid ID token", err.Error())
		return
	}
	if idt.Nonce != parts[1] {
		e.denied(ctx, w, "", "invalid nonce", "")
		return
	}
	var cl entraClaims
	if err := idt.Claims(&cl); err != nil {
		e.denied(ctx, w, "", "unreadable claims", err.Error())
		return
	}
	who := firstNonEmpty(cl.Email, cl.PreferredUsername, cl.OID)
	if e.cfg.CheckTenant && cl.TID != e.cfg.TenantID {
		e.denied(ctx, w, who, "wrong tenant", "tid="+cl.TID)
		return
	}
	if _, overage := cl.ClaimNames["groups"]; overage {
		// More than 200 groups: Entra does not put them in the token. We do
		// not call Microsoft Graph on purpose (one less permission).
		e.denied(ctx, w, who, "too many groups in the token (overage)",
			"ask the Entra admin to set the app's groups claim to \"Groups assigned to the application\"")
		return
	}
	admin, viewer := e.groups()
	roles := RolesFor(cl.Groups, admin, viewer)
	if len(roles) == 0 {
		e.denied(ctx, w, who, "not in an allowed group", "ask the nautilm team to add you to the viewer or admin group")
		return
	}
	u := store.User{ID: cl.OID, Name: firstNonEmpty(cl.Name, who), Email: firstNonEmpty(cl.Email, cl.PreferredUsername),
		Roles: roles, Groups: cl.Groups}
	cookie, err := e.store.CreateSession(ctx, u, e.cfg.SessionTTL)
	if err != nil {
		e.log.Error("creating session", "error", err)
		page(w, http.StatusServiceUnavailable, "Sign-in unavailable", "the session could not be saved (database).")
		return
	}
	e.audit(ctx, store.AuditEntry{Actor: u.Email, Action: "login", Target: u.ID, Details: map[string]any{"roles": roles}})
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: cookie, Path: "/", MaxAge: int(e.cfg.SessionTTL.Seconds()),
		HttpOnly: true, Secure: e.secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

// RolesFor maps the user's groups to roles.
func RolesFor(groups, adminGroups, viewerGroups []string) []string {
	in := func(set []string) bool {
		for _, g := range groups {
			if slices.Contains(set, g) {
				return true
			}
		}
		return false
	}
	switch {
	case in(adminGroups):
		return []string{RoleAdmin, RoleViewer}
	case in(viewerGroups):
		return []string{RoleViewer}
	}
	return nil
}

// Logout ends the session (in the database too).
func (e *Entra) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		if s, err := e.store.GetSession(r.Context(), c.Value); err == nil {
			e.audit(r.Context(), store.AuditEntry{Actor: s.Email, Action: "logout", Target: s.ID})
		}
		e.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: e.secure(), SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

// SessionFrom returns the signed-in user, if any.
func (e *Entra) SessionFrom(r *http.Request) (store.Session, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return store.Session{}, false
	}
	s, err := e.store.GetSession(r.Context(), c.Value)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			e.log.Error("reading session", "error", err)
		}
		return store.Session{}, false
	}
	// Roles follow the group map in effect, not the one at sign-in time:
	// removing a group in the Config tab takes effect on the next request.
	// (Sessions from before groups were stored keep their roles until they expire.)
	if len(s.Groups) > 0 {
		admin, viewer := e.groups()
		s.Roles = RolesFor(s.Groups, admin, viewer)
		if len(s.Roles) == 0 {
			return store.Session{}, false
		}
	}
	return s, true
}

// SameOrigin is the CSRF check for state-changing requests made with the
// session cookie: the browser must say the request comes from the dashboard.
func (e *Entra) SameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == e.cfg.PublicURL
}

func (e *Entra) denied(ctx context.Context, w http.ResponseWriter, who, reason, detail string) {
	e.log.Warn("sign-in denied", "user", who, "reason", reason, "detail", detail)
	e.audit(ctx, store.AuditEntry{Actor: firstNonEmpty(who, "unknown"), Action: "login.denied", Target: reason, Details: map[string]any{"detail": detail}})
	msg := "Reason: " + reason + "."
	if detail != "" && reason != "code exchange failed" && reason != "invalid ID token" {
		msg += " " + detail + "."
	}
	page(w, http.StatusForbidden, "Access denied", msg)
}

func (e *Entra) audit(ctx context.Context, a store.AuditEntry) {
	if err := e.store.Audit(ctx, a); err != nil {
		e.log.Error("audit write failed", "error", err, "action", a.Action, "actor", a.Actor)
	}
}

var pageTmpl = template.Must(template.New("p").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}} · Nautilus</title>
<style>body{margin:0;background:#0b0c0e;color:#fff;font-family:system-ui,-apple-system,"Segoe UI",sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px}
.box{background:#15171b;border:1px solid rgba(255,255,255,.09);border-radius:12px;padding:24px;max-width:460px}
p{color:#c3c2b7}a{color:#3987e5}</style></head><body><div class="box"><h1>{{.Title}}</h1><p>{{.Msg}}</p>
<p><a href="/dashboard">Back to the dashboard</a></p></div></body></html>`))

func page(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	pageTmpl.Execute(w, map[string]string{"Title": title, "Msg": msg})
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
