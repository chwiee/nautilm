package store

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"
)

// Memory is an in-process Store for unit tests. Not for production: it
// forgets everything when the process stops.
type Memory struct {
	mu        sync.Mutex
	nextID    int64
	tokens    map[int64]*Token
	byHash    map[string]int64
	sessions  map[string]Session
	audit     []AuditEntry
	revisions []Revision
	Now       func() time.Time
}

func NewMemory() *Memory {
	return &Memory{tokens: map[int64]*Token{}, byHash: map[string]int64{}, sessions: map[string]Session{}, Now: time.Now}
}

func (m *Memory) CreateToken(_ context.Context, n NewToken, actor string) (string, Token, error) {
	if err := n.validate(); err != nil {
		return "", Token{}, err
	}
	clear, hash := newSecret(TokenPrefix)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	now := m.Now().UTC()
	t := &Token{ID: m.nextID, Agent: n.Agent, Prefix: displayPrefix(clear), Team: n.Team, CostCenter: n.CostCenter,
		Models: slices.Clone(n.Models), TokensPerMinute: n.TokensPerMinute, TokensPerDay: n.TokensPerDay,
		Note: n.Note, CreatedBy: actor, CreatedAt: now, ExpiresAt: now.Add(n.TTL)}
	m.tokens[t.ID] = t
	m.byHash[hash] = t.ID
	return clear, *t, nil
}

func (m *Memory) ListTokens(context.Context) ([]Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Token, 0, len(m.tokens))
	for _, t := range m.tokens {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (m *Memory) GetToken(_ context.Context, id int64) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return Token{}, ErrNotFound
	}
	return *t, nil
}

func (m *Memory) RevokeToken(_ context.Context, id int64, actor string) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return Token{}, ErrNotFound
	}
	if t.RevokedAt == nil {
		now := m.Now().UTC()
		t.RevokedAt, t.RevokedBy = &now, actor
	}
	return *t, nil
}

func (m *Memory) TokenByHash(_ context.Context, hash string) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byHash[hash]
	if !ok {
		return Token{}, ErrNotFound
	}
	return *m.tokens[id], nil
}

func (m *Memory) TouchToken(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tokens[id]; ok {
		at = at.UTC()
		t.LastUsedAt = &at
	}
	return nil
}

func (m *Memory) CreateSession(_ context.Context, u User, ttl time.Duration) (string, error) {
	clear, hash := newSecret("")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[hash] = Session{User: u, ExpiresAt: m.Now().Add(ttl)}
	return clear, nil
}

func (m *Memory) GetSession(_ context.Context, cookie string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[Hash(cookie)]
	if !ok || m.Now().After(s.ExpiresAt) {
		return Session{}, ErrNotFound
	}
	return s, nil
}

func (m *Memory) DeleteSession(_ context.Context, cookie string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, Hash(cookie))
	return nil
}

func (m *Memory) Audit(_ context.Context, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = int64(len(m.audit) + 1)
	if e.At.IsZero() {
		e.At = m.Now().UTC()
	}
	m.audit = append(m.audit, e)
	return nil
}

func (m *Memory) ListAudit(_ context.Context, limit int) ([]AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.audit)
	slices.Reverse(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) SaveRevision(_ context.Context, n NewRevision, actor string) (Revision, error) {
	if err := n.validate(); err != nil {
		return Revision{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest int64
	for _, r := range m.revisions {
		if r.Kind == n.Kind {
			latest = r.ID
		}
	}
	if latest != n.Parent {
		return Revision{}, ErrConflict
	}
	r := Revision{ID: int64(len(m.revisions) + 1), Kind: n.Kind, Payload: slices.Clone(n.Payload), Actor: actor,
		Note: n.Note, CreatedAt: m.Now().UTC()}
	if n.RollbackOf > 0 {
		rb := n.RollbackOf
		r.RollbackOf = &rb
	}
	m.revisions = append(m.revisions, r)
	return r, nil
}

func (m *Memory) LatestRevisions(context.Context) (map[string]Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Revision{}
	for _, r := range m.revisions {
		out[r.Kind] = r
	}
	return out, nil
}

func (m *Memory) ListRevisions(_ context.Context, kind string, limit int) ([]Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Revision{}
	for i := len(m.revisions) - 1; i >= 0; i-- {
		if m.revisions[i].Kind == kind {
			out = append(out, m.revisions[i])
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) GetRevision(_ context.Context, id int64) (Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id <= 0 || int(id) > len(m.revisions) {
		return Revision{}, ErrNotFound
	}
	return m.revisions[id-1], nil
}

func (m *Memory) LastRevisionID(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.revisions)), nil
}

func (m *Memory) Ping(context.Context) error { return nil }
func (m *Memory) Close()                     {}
