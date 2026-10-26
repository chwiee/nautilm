package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/chwiee/nautilm/internal/store"
)

func req(token string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

type downStore struct{ store.Store }

func (downStore) TokenByHash(context.Context, string) (store.Token, error) {
	return store.Token{}, errors.New("connection refused")
}

func TestStoredTokens(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	clear, _, _ := mem.CreateToken(ctx, store.NewToken{Agent: "billing-app", Team: "finance", Models: []string{"fast"}, TTL: time.Hour}, "ana")
	a := NewStoredTokens(mem)

	id, err := a.Authenticate(req(clear))
	if err != nil || id.Agent != "billing-app" || id.Method != "stored-token" || id.Profile == nil || id.Profile.Team != "finance" {
		t.Fatalf("valid token: %+v %v", id, err)
	}
	// Not a nautilm token: let the next authenticator try.
	if _, err := a.Authenticate(req("some-serviceaccount-jwt")); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("foreign token: %v", err)
	}
	if _, err := a.Authenticate(req(store.TokenPrefix + "unknown")); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown naut token: %v", err)
	}

	// Revocation: cached for TTL, gone right away after Forget.
	toks, _ := mem.ListTokens(ctx)
	mem.RevokeToken(ctx, toks[0].ID, "bob")
	if _, err := a.Authenticate(req(clear)); err != nil {
		t.Errorf("within the cache TTL the token still works (documented trade-off): %v", err)
	}
	a.Forget()
	if _, err := a.Authenticate(req(clear)); !errors.Is(err, ErrInvalid) {
		t.Errorf("after Forget the revoked token must be invalid: %v", err)
	}

	// Expired.
	exp, _, _ := mem.CreateToken(ctx, store.NewToken{Agent: "x", Models: []string{"fast"}, TTL: time.Hour}, "ana")
	a2 := NewStoredTokens(mem)
	a2.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := a2.Authenticate(req(exp)); !errors.Is(err, ErrInvalid) {
		t.Errorf("expired: %v", err)
	}

	// Database down: an error (503 upstream), NOT "invalid credentials" (401).
	down := NewStoredTokens(downStore{mem})
	if _, err := down.Authenticate(req(clear)); err == nil || errors.Is(err, ErrInvalid) || errors.Is(err, ErrNoCredentials) {
		t.Errorf("database down: %v", err)
	}
}
