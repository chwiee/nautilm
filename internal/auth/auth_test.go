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
		t.Errorf("token vazio no config não pode autenticar ninguém: %v", err)
	}
}

// Simula o API server respondendo TokenReview.
func TestKubernetesTokenReview(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer token-do-pedagio" {
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
		ok := in.Spec.Token == "sa-kubets" && len(in.Spec.Audiences) == 1 && in.Spec.Audiences[0] == "pedagio"
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{
			"authenticated": ok,
			"user":          map[string]any{"username": "system:serviceaccount:observability:kubets"},
		}})
	}))
	defer srv.Close()

	k := &Kubernetes{APIServer: srv.URL, Audience: "pedagio", http: srv.Client(),
		readToken: func() (string, error) { return "token-do-pedagio", nil }}

	id, err := k.Authenticate(reqWith("sa-kubets"))
	if err != nil || id.Agent != "observability/kubets" {
		t.Fatalf("id=%+v err=%v", id, err)
	}
	k.Authenticate(reqWith("sa-kubets")) // deve vir do cache
	if calls != 1 {
		t.Errorf("esperava 1 TokenReview (cache), houve %d", calls)
	}
	if _, err := k.Authenticate(reqWith("forjado")); !errors.Is(err, ErrInvalid) {
		t.Errorf("token forjado: %v", err)
	}
}
