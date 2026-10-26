package webauth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chwiee/nautilm/internal/oidcmock"
	"github.com/chwiee/nautilm/internal/store"
)

type env struct {
	app   *httptest.Server
	store *store.Memory
	entra *Entra
}

func setup(t *testing.T) env {
	t.Helper()
	idp := httptest.NewUnstartedServer(nil)
	idp.Start()
	t.Cleanup(idp.Close)
	mock, err := oidcmock.New(oidcmock.Options{Tenant: "tenant-1", ClientID: "nautilm-app", ClientSecret: "s3cret",
		PublicBase: idp.URL, InternalBase: idp.URL, Users: oidcmock.DefaultUsers("grp-admin", "grp-viewer")})
	if err != nil {
		t.Fatal(err)
	}
	idp.Config.Handler = mock.Handler()

	st := store.NewMemory()
	app := httptest.NewUnstartedServer(nil)
	e := New(Config{Issuer: mock.Issuer(), TenantID: "tenant-1", ClientID: "nautilm-app", ClientSecret: "s3cret",
		PublicURL: "http://" + app.Listener.Addr().String(), AdminGroups: []string{"grp-admin"}, ViewerGroups: []string{"grp-viewer"},
		SessionTTL: time.Hour, CheckTenant: true}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", e.Login)
	mux.HandleFunc("GET /auth/callback", e.Callback)
	mux.HandleFunc("POST /auth/logout", e.Logout)
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		s, ok := e.SessionFrom(r)
		if !ok {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(s)
	})
	app.Config.Handler = mux
	app.Start()
	t.Cleanup(app.Close)
	return env{app: app, store: st, entra: e}
}

// signIn runs the browser side: login → mock page → pick user → callback → dashboard.
func signIn(t *testing.T, e env, user string) (*http.Client, *http.Response) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	// Stop at the mock sign-in page to "click" the user.
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if strings.Contains(req.URL.Path, "/authorize") && req.URL.Query().Get("user") == "" {
			q := req.URL.Query()
			q.Set("user", user)
			req.URL.RawQuery = q.Encode()
		}
		return nil
	}
	resp, err := c.Get(e.app.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return c, resp
}

func TestAdminAndViewerSignIn(t *testing.T) {
	e := setup(t)
	for user, roles := range map[string]string{"ana": "admin,viewer", "bruno": "viewer"} {
		_, resp := signIn(t, e, user)
		var s store.Session
		json.NewDecoder(resp.Body).Decode(&s)
		if resp.StatusCode != 200 || strings.Join(s.Roles, ",") != roles || s.ID != "oid-"+user {
			t.Errorf("%s: status %d session %+v", user, resp.StatusCode, s)
		}
	}
	a, _ := e.store.ListAudit(context.Background(), 10)
	if len(a) != 2 || a[0].Action != "login" {
		t.Errorf("audit: %+v", a)
	}
}

func TestDeniedSignIns(t *testing.T) {
	e := setup(t)
	for user, want := range map[string]string{
		"carla": "not in an allowed group",
		"dani":  "too many groups", // overage: the token has no "groups" claim
	} {
		_, resp := signIn(t, e, user)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 403 || !strings.Contains(string(body), want) {
			t.Errorf("%s: status %d body %s", user, resp.StatusCode, body)
		}
	}
	a, _ := e.store.ListAudit(context.Background(), 10)
	if len(a) != 2 || a[0].Action != "login.denied" {
		t.Errorf("denied sign-ins must be audited: %+v", a)
	}
}

func TestForgedStateIsRejected(t *testing.T) {
	e := setup(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	// A callback without the flow cookie (e.g. a link sent by an attacker).
	resp, err := c.Get(e.app.URL + "/auth/callback?code=x&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("forged state: %d", resp.StatusCode)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	e := setup(t)
	c, _ := signIn(t, e, "ana")
	u, _ := url.Parse(e.app.URL)
	if len(c.Jar.Cookies(u)) == 0 {
		t.Fatal("no session cookie")
	}
	req, _ := http.NewRequest(http.MethodPost, e.app.URL+"/auth/logout", nil)
	if r, err := c.Do(req); err != nil || r.StatusCode != 204 {
		t.Fatalf("logout: %v %v", r, err)
	}
	r, _ := c.Get(e.app.URL + "/dashboard")
	if r.StatusCode != 401 {
		t.Errorf("after logout: %d", r.StatusCode)
	}
}

func TestRolesFor(t *testing.T) {
	if r := RolesFor([]string{"x", "a"}, []string{"a"}, []string{"v"}); strings.Join(r, ",") != "admin,viewer" {
		t.Error(r)
	}
	if r := RolesFor(nil, []string{"a"}, []string{"v"}); r != nil {
		t.Error("no groups = no roles")
	}
}
