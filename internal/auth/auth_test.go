package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqWith(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestTokens(t *testing.T) {
	a := NewTokens(map[string]string{"ce-cli": "segredo", "vazio": ""})
	if id, err := a.Authenticate(reqWith("segredo")); err != nil || id.Agent != "ce-cli" {
		t.Errorf("id=%+v err=%v", id, err)
	}
	if _, err := a.Authenticate(reqWith("")); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("an empty token in the config must not authenticate anyone: %v", err)
	}
}

// Simulates the API server answering a TokenReview.
func TestKubernetesTokenReview(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer nautilm-own-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var in struct {
			Spec struct {
				Token     string   `json:"token"`
				Audiences []string `json:"audiences"`
			} `json:"spec"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		ok := in.Spec.Token == "h.sa-kubets.s" && len(in.Spec.Audiences) == 1 && in.Spec.Audiences[0] == "nautilm"
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{
			"authenticated": ok,
			"user":          map[string]any{"username": "system:serviceaccount:observability:kubets"},
		}})
	}))
	defer srv.Close()

	k := &Kubernetes{APIServer: srv.URL, Audience: "nautilm", http: srv.Client(),
		readToken: func() (string, error) { return "nautilm-own-token", nil }}

	id, err := k.Authenticate(reqWith("h.sa-kubets.s"))
	if err != nil || id.Agent != "observability/kubets" {
		t.Fatalf("id=%+v err=%v", id, err)
	}
	k.Authenticate(reqWith("h.sa-kubets.s")) // deve vir do cache
	if calls != 1 {
		t.Errorf("esperava 1 TokenReview (cache), houve %d", calls)
	}
	if _, err := k.Authenticate(reqWith("h.forjado.s")); !errors.Is(err, ErrInvalid) {
		t.Errorf("token forjado: %v", err)
	}
}

// auth.modes: [kubernetes, tokens] (the example config): a static token is
// not a JWT, so the Kubernetes authenticator must let it through to "tokens"
// instead of rejecting it — and must not even call the API server.
func TestStaticTokenAfterKubernetes(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"authenticated": false}})
	}))
	defer srv.Close()
	k := &Kubernetes{APIServer: srv.URL, Audience: "nautilm", http: srv.Client(),
		readToken: func() (string, error) { return "own", nil }}
	chain := Chain{k, NewTokens(map[string]string{"marcia": "Xk29sQpL0aZ"})}

	id, err := chain.Authenticate(reqWith("Xk29sQpL0aZ"))
	if err != nil || id.Agent != "marcia" || id.Method != "tokens" {
		t.Fatalf("static token after kubernetes: id=%+v err=%v", id, err)
	}
	if calls != 0 {
		t.Errorf("a non-JWT token must not cost a TokenReview (%d calls)", calls)
	}
	// A JWT-shaped token that the API server rejects is still invalid (no fall-through).
	if _, err := chain.Authenticate(reqWith("aaa.bbb.ccc")); !errors.Is(err, ErrInvalid) {
		t.Errorf("rejected ServiceAccount token: %v", err)
	}
	for _, s := range []string{"", "a.b", "a..c", "a.b.c.d", "naut_abc", "a.b c.d"} {
		if looksLikeJWT(s) {
			t.Errorf("%q is not a JWT", s)
		}
	}
}
