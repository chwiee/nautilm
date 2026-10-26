package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// conformance runs the same scenario against any Store.
func conformance(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()

	// Validation.
	if _, _, err := s.CreateToken(ctx, NewToken{Agent: "", Models: []string{"fast"}, TTL: time.Hour}, "ana"); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty agent must be invalid: %v", err)
	}
	if _, _, err := s.CreateToken(ctx, NewToken{Agent: "x", TTL: time.Hour}, "ana"); !errors.Is(err, ErrInvalid) {
		t.Errorf("no models must be invalid: %v", err)
	}

	clear, tok, err := s.CreateToken(ctx, NewToken{Agent: "billing-app", Team: "finance", CostCenter: "CC-2040",
		Models: []string{"fast", "embed"}, TokensPerDay: 50000, Note: "monthly report", TTL: 90 * 24 * time.Hour}, "ana@corp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(clear, TokenPrefix) || len(clear) < 40 || tok.Prefix != clear[:len(tok.Prefix)] {
		t.Errorf("token format: %q prefix %q", clear, tok.Prefix)
	}
	if tok.ID == 0 || tok.CreatedBy != "ana@corp" || !tok.Active(time.Now()) || len(tok.Models) != 2 {
		t.Errorf("created: %+v", tok)
	}

	// Lookup by hash (auth path); the clear text is not stored anywhere.
	got, err := s.TokenByHash(ctx, Hash(clear))
	if err != nil || got.ID != tok.ID || got.Agent != "billing-app" || got.TokensPerDay != 50000 {
		t.Fatalf("by hash: %+v %v", got, err)
	}
	if _, err := s.TokenByHash(ctx, Hash(clear+"x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown hash: %v", err)
	}

	// Second token for the same agent (rotation keeps both valid).
	clear2, tok2, _ := s.CreateToken(ctx, NewToken{Agent: "billing-app", Models: []string{"fast"}, TTL: time.Hour}, "ana@corp")
	if clear2 == clear || tok2.ID == tok.ID {
		t.Error("tokens must differ")
	}
	list, _ := s.ListTokens(ctx)
	if len(list) != 2 || list[0].ID != tok2.ID {
		t.Errorf("list (newest first): %+v", list)
	}

	if err := s.TouchToken(ctx, tok.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GetToken(ctx, tok.ID); g.LastUsedAt == nil {
		t.Error("last used not recorded")
	}

	// Revoke: immediate, idempotent, keeps who did it.
	r, err := s.RevokeToken(ctx, tok.ID, "bob@corp")
	if err != nil || r.RevokedAt == nil || r.RevokedBy != "bob@corp" || r.Active(time.Now()) {
		t.Fatalf("revoke: %+v %v", r, err)
	}
	r2, _ := s.RevokeToken(ctx, tok.ID, "someone-else")
	if r2.RevokedBy != "bob@corp" {
		t.Error("revoking twice must keep the first revoker")
	}
	if _, err := s.RevokeToken(ctx, 999999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke unknown: %v", err)
	}

	// Sessions.
	cookie, err := s.CreateSession(ctx, User{ID: "oid-1", Name: "Ana", Email: "ana@corp", Roles: []string{"admin", "viewer"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.GetSession(ctx, cookie)
	if err != nil || sess.Email != "ana@corp" || len(sess.Roles) != 2 {
		t.Fatalf("session: %+v %v", sess, err)
	}
	if _, err := s.GetSession(ctx, cookie+"x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong cookie: %v", err)
	}
	s.DeleteSession(ctx, cookie)
	if _, err := s.GetSession(ctx, cookie); !errors.Is(err, ErrNotFound) {
		t.Error("deleted session must be gone")
	}
	expired, _ := s.CreateSession(ctx, User{ID: "oid-2", Name: "Old", Email: "old@corp", Roles: []string{"viewer"}}, -time.Minute)
	if _, err := s.GetSession(ctx, expired); !errors.Is(err, ErrNotFound) {
		t.Error("expired session must not be valid")
	}

	// Audit, newest first.
	s.Audit(ctx, AuditEntry{Actor: "ana@corp", Action: "token.create", Target: "billing-app", Details: map[string]any{"id": tok.ID}})
	s.Audit(ctx, AuditEntry{Actor: "bob@corp", Action: "token.revoke", Target: "billing-app"})
	a, _ := s.ListAudit(ctx, 10)
	if len(a) != 2 || a[0].Action != "token.revoke" || a[1].Details["id"] == nil {
		t.Errorf("audit: %+v", a)
	}
}

func TestMemory(t *testing.T) { conformance(t, NewMemory()) }

// Runs against a real Postgres when NAUTILM_TEST_DATABASE_URL is set, e.g.
// docker run -d --name nautilm-pg-test -e POSTGRES_PASSWORD=test -p 15432:5432 postgres:17-alpine
// NAUTILM_TEST_DATABASE_URL=postgres://postgres:test@localhost:15432/postgres?sslmode=disable
func TestPostgres(t *testing.T) {
	url := os.Getenv("NAUTILM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("NAUTILM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	p, err := OpenPostgres(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, tbl := range []string{"api_tokens", "sessions", "audit_log"} {
		p.pool.Exec(ctx, "TRUNCATE "+tbl)
	}
	conformance(t, p)

	// Opening again must not re-run migrations (replica restart).
	p2, err := OpenPostgres(ctx, url)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	p2.Close()
}
