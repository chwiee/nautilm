package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chwiee/nautilm/internal/store"
)

// Profile is what a token created from the dashboard carries: the agent's
// permissions and billing, stored with the token instead of the YAML.
type Profile struct {
	Models          []string
	Team            string
	CostCenter      string
	TokensPerMinute int64
	TokensPerDay    int64
}

// StoredTokens authenticates "naut_..." tokens created from the dashboard.
//
// A short cache (TTL) avoids one database query per LLM call. Trade-off: a
// revoked token may keep working for up to TTL on a replica that saw it
// recently; Forget clears it right away on the replica that revoked it.
type StoredTokens struct {
	Store store.Store
	TTL   time.Duration
	Now   func() time.Time
	// OnError is called when the database fails (logging).
	OnError func(error)

	mu      sync.Mutex
	cache   map[string]cachedToken
	touched map[int64]time.Time
}

type cachedToken struct {
	tok   store.Token
	found bool
	until time.Time
}

func NewStoredTokens(s store.Store) *StoredTokens {
	return &StoredTokens{Store: s, TTL: 15 * time.Second, Now: time.Now, cache: map[string]cachedToken{}, touched: map[int64]time.Time{}}
}

func (s *StoredTokens) Authenticate(r *http.Request) (Identity, error) {
	tok := BearerToken(r)
	if !strings.HasPrefix(tok, store.TokenPrefix) {
		return Identity{}, ErrNoCredentials // not ours: let the other authenticators try
	}
	hash := store.Hash(tok)
	now := s.Now()
	t, found, err := s.lookup(r.Context(), hash, now)
	if err != nil {
		if s.OnError != nil {
			s.OnError(err)
		}
		return Identity{}, err // database down: 503 (not 401)
	}
	if !found || !t.Active(now) {
		return Identity{}, ErrInvalid
	}
	s.touch(t.ID, now)
	return Identity{Agent: t.Agent, Method: "stored-token", TokenID: t.ID, Profile: &Profile{
		Models: t.Models, Team: t.Team, CostCenter: t.CostCenter,
		TokensPerMinute: t.TokensPerMinute, TokensPerDay: t.TokensPerDay,
	}}, nil
}

func (s *StoredTokens) lookup(ctx context.Context, hash string, now time.Time) (store.Token, bool, error) {
	s.mu.Lock()
	c, ok := s.cache[hash]
	s.mu.Unlock()
	if ok && now.Before(c.until) {
		return c.tok, c.found, nil
	}
	t, err := s.Store.TokenByHash(ctx, hash)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return t, false, err
	}
	s.mu.Lock()
	if len(s.cache) > 10_000 {
		s.cache = map[string]cachedToken{}
	}
	s.cache[hash] = cachedToken{tok: t, found: found, until: now.Add(s.TTL)}
	s.mu.Unlock()
	return t, found, nil
}

// touch records "last used" at most once a minute per token, in the
// background, so the LLM call is not slowed down.
func (s *StoredTokens) touch(id int64, now time.Time) {
	s.mu.Lock()
	last := s.touched[id]
	if now.Sub(last) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.touched[id] = now
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Store.TouchToken(ctx, id, now); err != nil && s.OnError != nil {
			s.OnError(err)
		}
	}()
}

// Forget drops every cached entry (called after a revocation on this replica).
func (s *StoredTokens) Forget() {
	s.mu.Lock()
	s.cache = map[string]cachedToken{}
	s.mu.Unlock()
}
