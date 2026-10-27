package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chwiee/nautilm/internal/auth"
	"github.com/chwiee/nautilm/internal/config"
	"github.com/chwiee/nautilm/internal/live"
	"github.com/chwiee/nautilm/internal/metrics"
	"github.com/chwiee/nautilm/internal/oidcmock"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/internal/store"
	"github.com/chwiee/nautilm/internal/usage"
	"github.com/chwiee/nautilm/internal/webauth"

	"github.com/aws/smithy-go"
)

type adminEnv struct {
	srv     *httptest.Server
	store   *store.Memory
	secrets fakeSecrets
	gw      *Gateway
}

// fakeSecrets stands in for Secrets Manager: arn → value or error.
type fakeSecrets map[string]any

func (f fakeSecrets) Read(_ context.Context, arn string) (string, error) {
	switch v := f[arn].(type) {
	case string:
		return v, nil
	case error:
		return "", v
	}
	return "", &smithy.GenericAPIError{Code: "ResourceNotFoundException", Message: "Secrets Manager can't find the specified secret."}
}

// setupAdmin: gateway with database (memory), Entra (mock) and stored tokens.
func setupAdmin(t *testing.T) adminEnv {
	t.Helper()
	idp := httptest.NewServer(nil)
	t.Cleanup(idp.Close)
	mock, _ := oidcmock.New(oidcmock.Options{Tenant: "t1", ClientID: "app", ClientSecret: "s", PublicBase: idp.URL,
		InternalBase: idp.URL, Users: oidcmock.DefaultUsers("grp-admin", "grp-viewer")})
	idp.Config.Handler = mock.Handler()

	srv := httptest.NewUnstartedServer(nil)
	public := "http://" + srv.Listener.Addr().String()
	base := strings.Replace(testConfig, "dashboard: {token: dash-secret}\n", "", 1)
	cfg, err := config.Parse([]byte(base + fmt.Sprintf(`
database: {url: "memory"}
dashboard:
  token: dash-secret
  public_url: %s
  entra: {tenant_id: t1, client_id: app, client_secret: s, admin_groups: [grp-admin], viewer_groups: [grp-viewer], issuer_url: %s}
`, public, mock.Issuer())))
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	stored := auth.NewStoredTokens(st)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := New(cfg, map[string]provider.Provider{"a": &fake{}, "b": &fake{}},
		auth.Chain{stored, auth.NewTokens(cfg.Auth.Tokens)}, metrics.New(), log)
	gw.UseStore(st, stored)
	secrets := fakeSecrets{}
	mgr := &live.Manager{Base: cfg, BaseProviders: gw.static.Providers, Store: st, Secrets: secrets,
		Build: live.DefaultBuilder, Log: log, Interval: time.Hour}
	mgr.Start(context.Background())
	gw.UseConfigManager(mgr)
	gw.UseEntra(webauth.New(webauth.Config{Issuer: mock.Issuer(), TenantID: "t1", ClientID: "app", ClientSecret: "s",
		PublicURL: public, AdminGroups: []string{"grp-admin"}, ViewerGroups: []string{"grp-viewer"}, SessionTTL: time.Hour, CheckTenant: true,
		Groups: func() ([]string, []string) {
			e := mgr.Current().Cfg.Dashboard.Entra
			return e.AdminGroups, e.ViewerGroups
		}}, st, log))
	srv.Config.Handler = gw.Handler()
	srv.Start()
	t.Cleanup(srv.Close)
	return adminEnv{srv: srv, store: st, secrets: secrets, gw: gw}
}

// browser signs in as the mock user and returns a client with the session cookie.
func browser(t *testing.T, e adminEnv, user string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if strings.Contains(req.URL.Path, "/authorize") && req.URL.Query().Get("user") == "" {
			q := req.URL.Query()
			q.Set("user", user)
			req.URL.RawQuery = q.Encode()
		}
		return nil
	}}
	resp, err := c.Get(e.srv.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return c
}

func send(t *testing.T, c *http.Client, method, url, origin string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestTokenLifecycleFromTheDashboard(t *testing.T) {
	e := setupAdmin(t)
	admin := browser(t, e, "ana")
	origin := e.srv.URL

	// Who am I?
	_, me := send(t, admin, http.MethodGet, e.srv.URL+"/auth/me", "", nil)
	if u, _ := me["user"].(map[string]any); u == nil || !strings.Contains(fmt.Sprint(u["roles"]), "admin") {
		t.Fatalf("me: %+v", me)
	}

	// Create a token for an app outside the cluster.
	resp, out := send(t, admin, http.MethodPost, e.srv.URL+"/v1/admin/tokens", origin, map[string]any{
		"agent": "billing-app", "team": "finance", "cost_center": "CC-2040", "models": []string{"fast"}, "tokens_per_day": 1000})
	clear, _ := out["token"].(string)
	if resp.StatusCode != 201 || !strings.HasPrefix(clear, "naut_") {
		t.Fatalf("create: %d %+v", resp.StatusCode, out)
	}
	info := out["info"].(map[string]any)
	id := int64(info["id"].(float64))

	// The app uses it right away (no restart): allowed alias works, others do not.
	if r := post(t, e.srv, clear, fmt.Sprintf(hello, "fast")); r.StatusCode != 200 || r.Header.Get("X-Nautilm-Billed-To") != "billing-app" {
		t.Fatalf("call with the new token: %d %v", r.StatusCode, r.Header)
	}
	if r := post(t, e.srv, clear, fmt.Sprintf(hello, "smart")); r.StatusCode != 403 {
		t.Errorf("alias not in the token profile must be 403: %d", r.StatusCode)
	}
	// Billing uses the token's team and cost center.
	r, _ := send(t, admin, http.MethodGet, e.srv.URL+"/v1/usage?group_by=agent,team,cost_center", "", nil)
	if r.StatusCode != 200 {
		t.Fatalf("usage as admin: %d", r.StatusCode)
	}
	var rep usage.Report
	resp2, _ := admin.Get(e.srv.URL + "/v1/usage?group_by=agent,team,cost_center")
	json.NewDecoder(resp2.Body).Decode(&rep)
	resp2.Body.Close()
	if len(rep.Groups) != 1 || rep.Groups[0].Key["team"] != "finance" || rep.Groups[0].Key["cost_center"] != "CC-2040" {
		t.Errorf("ledger: %+v", rep.Groups)
	}

	// The clear text is never listed again.
	_, list := send(t, admin, http.MethodGet, e.srv.URL+"/v1/admin/tokens", "", nil)
	if b, _ := json.Marshal(list); strings.Contains(string(b), clear) {
		t.Fatal("the clear-text token must never be listed")
	}

	// Rotation: new token works, the old one too (no downtime) until revoked.
	resp, rot := send(t, admin, http.MethodPost, fmt.Sprintf("%s/v1/admin/tokens/%d/rotate", e.srv.URL, id), origin, nil)
	newClear, _ := rot["token"].(string)
	if resp.StatusCode != 201 || newClear == "" || newClear == clear {
		t.Fatalf("rotate: %d %+v", resp.StatusCode, rot)
	}
	for _, tok := range []string{clear, newClear} {
		if r := post(t, e.srv, tok, fmt.Sprintf(hello, "fast")); r.StatusCode != 200 {
			t.Errorf("during rotation both tokens work: %d", r.StatusCode)
		}
	}

	// Revoke the old one: denied right away on this replica.
	if resp, _ := send(t, admin, http.MethodPost, fmt.Sprintf("%s/v1/admin/tokens/%d/revoke", e.srv.URL, id), origin, nil); resp.StatusCode != 200 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if r := post(t, e.srv, clear, fmt.Sprintf(hello, "fast")); r.StatusCode != 401 {
		t.Errorf("revoked token must be 401: %d", r.StatusCode)
	}
	if r := post(t, e.srv, newClear, fmt.Sprintf(hello, "fast")); r.StatusCode != 200 {
		t.Errorf("the new token keeps working: %d", r.StatusCode)
	}

	// Audit trail: who did what.
	_, a := send(t, admin, http.MethodGet, e.srv.URL+"/v1/admin/audit", "", nil)
	got := fmt.Sprint(a["entries"])
	for _, action := range []string{"login", "token.create", "token.rotate", "token.revoke"} {
		if !strings.Contains(got, action) || !strings.Contains(got, "ana@corp.example") {
			t.Errorf("audit missing %s: %s", action, got)
		}
	}
}

func TestAdminProtections(t *testing.T) {
	e := setupAdmin(t)
	admin, viewer := browser(t, e, "ana"), browser(t, e, "bruno")
	body := map[string]any{"agent": "x-app", "models": []string{"fast"}}

	// Viewer: sees usage, cannot manage tokens.
	if r, _ := send(t, viewer, http.MethodGet, e.srv.URL+"/v1/usage", "", nil); r.StatusCode != 200 {
		t.Errorf("viewer reads usage: %d", r.StatusCode)
	}
	if r, _ := send(t, viewer, http.MethodPost, e.srv.URL+"/v1/admin/tokens", e.srv.URL, body); r.StatusCode != 403 {
		t.Errorf("viewer creating a token: %d", r.StatusCode)
	}
	// Not signed in.
	if r, _ := send(t, http.DefaultClient, http.MethodGet, e.srv.URL+"/v1/admin/tokens", "", nil); r.StatusCode != 401 {
		t.Errorf("anonymous: %d", r.StatusCode)
	}
	// CSRF: admin cookie but the request does not come from the dashboard.
	for _, origin := range []string{"", "https://evil.example"} {
		if r, _ := send(t, admin, http.MethodPost, e.srv.URL+"/v1/admin/tokens", origin, body); r.StatusCode != 403 {
			t.Errorf("origin %q: %d", origin, r.StatusCode)
		}
	}
	// A name that exists in the YAML is refused (one place per agent).
	if r, _ := send(t, admin, http.MethodPost, e.srv.URL+"/v1/admin/tokens", e.srv.URL, map[string]any{"agent": "k8s", "models": []string{"fast"}}); r.StatusCode != 409 {
		t.Errorf("name from config: %d", r.StatusCode)
	}
	// Unknown alias.
	if r, _ := send(t, admin, http.MethodPost, e.srv.URL+"/v1/admin/tokens", e.srv.URL, map[string]any{"agent": "y", "models": []string{"gpt-9"}}); r.StatusCode != 400 {
		t.Errorf("unknown alias: %d", r.StatusCode)
	}
	// A dashboard token can never act on behalf of another agent.
	_, out := send(t, admin, http.MethodPost, e.srv.URL+"/v1/admin/tokens", e.srv.URL, body)
	clear, _ := out["token"].(string)
	r := post(t, e.srv, clear, fmt.Sprintf(hello, "fast"), HeaderOnBehalfOf, "k8s")
	if r.Header.Get("X-Nautilm-Billed-To") != "x-app" {
		t.Errorf("on-behalf-of from a dashboard token must be ignored: %v", r.Header)
	}
}
