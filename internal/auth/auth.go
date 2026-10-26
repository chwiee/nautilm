// Package auth finds out WHO is calling nautilm.
//
// Recommended mode on EKS: "kubernetes". The agent sends its own
// ServiceAccount token (projected with audience "nautilm") and nautilm asks
// the API server whether it is valid (TokenReview). Advantages:
//   - no secret to distribute or rotate;
//   - the identity is "namespace/serviceaccount", which already names the team/agent;
//   - the token expires by itself (the kubelet renews it).
//
// "tokens" is for callers outside the cluster (one token per agent, CLI, lab).
// "none" is for local development only.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// Identity is who is calling.
type Identity struct {
	Agent  string // e.g. "observability/kubets" or "k8s"
	Method string // kubernetes | tokens | none
}

var (
	// ErrNoCredentials: this authenticator does not apply (try the next one).
	ErrNoCredentials = errors.New("no credentials")
	// ErrInvalid: credentials present but invalid (stop: 401).
	ErrInvalid = errors.New("invalid credentials")
)

type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// Chain tries the authenticators in order.
type Chain []Authenticator

func (c Chain) Authenticate(r *http.Request) (Identity, error) {
	for _, a := range c {
		id, err := a.Authenticate(r)
		if errors.Is(err, ErrNoCredentials) {
			continue
		}
		return id, err
	}
	return Identity{}, ErrNoCredentials
}

// BearerToken extracts the token from the Authorization header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Tokens authenticates with static tokens (agent name → token).
// Only SHA-256 hashes are kept in memory.
type Tokens struct {
	byHash map[[32]byte]string
}

func NewTokens(m map[string]string) *Tokens {
	t := &Tokens{byHash: map[[32]byte]string{}}
	for name, tok := range m {
		if tok != "" {
			t.byHash[sha256.Sum256([]byte(tok))] = name
		}
	}
	return t
}

func (t *Tokens) Authenticate(r *http.Request) (Identity, error) {
	tok := BearerToken(r)
	if tok == "" {
		return Identity{}, ErrNoCredentials
	}
	h := sha256.Sum256([]byte(tok))
	// Compare hashes in constant time, so no timing is leaked.
	for k, name := range t.byHash {
		if subtle.ConstantTimeCompare(k[:], h[:]) == 1 {
			return Identity{Agent: name, Method: "tokens"}, nil
		}
	}
	// It may be a ServiceAccount token: let the next one try.
	return Identity{}, ErrNoCredentials
}

// None accepts everyone. The agent name comes from the X-Nautilm-Agent
// header (X-Pedagio-Agent is still accepted: the agentkit sends it).
// NEVER use it in production.
type None struct{}

func (None) Authenticate(r *http.Request) (Identity, error) {
	name := r.Header.Get("X-Nautilm-Agent")
	if name == "" {
		name = r.Header.Get("X-Pedagio-Agent")
	}
	if name == "" {
		name = "anonymous"
	}
	return Identity{Agent: name, Method: "none"}, nil
}
