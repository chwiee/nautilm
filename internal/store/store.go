// Package store keeps what nautilm must not lose: application tokens
// created from the dashboard, dashboard sessions and the audit trail.
//
// Tokens are never stored in clear text: only their SHA-256 hash. The clear
// text is shown ONCE, when the token is created (like GitHub personal access
// tokens). Whoever copies the database gets nothing usable.
//
// Two implementations: Postgres (production and the local cluster) and
// Memory (unit tests).
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// Token is an application token created from the dashboard. Each token
// carries its agent profile (what the agent may use and who pays).
type Token struct {
	ID              int64      `json:"id"`
	Agent           string     `json:"agent"`  // identity in nautilm (usage, quotas)
	Prefix          string     `json:"prefix"` // first characters, to recognize it in lists
	Team            string     `json:"team"`
	CostCenter      string     `json:"cost_center"`
	Models          []string   `json:"models"` // allowed aliases
	TokensPerMinute int64      `json:"tokens_per_minute"`
	TokensPerDay    int64      `json:"tokens_per_day"`
	Note            string     `json:"note"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	RevokedBy       string     `json:"revoked_by,omitempty"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
}

// Active tells whether the token can authenticate at "now".
func (t Token) Active(now time.Time) bool {
	return t.RevokedAt == nil && now.Before(t.ExpiresAt)
}

// NewToken is what an admin fills in to create a token.
type NewToken struct {
	Agent           string
	Team            string
	CostCenter      string
	Models          []string
	TokensPerMinute int64
	TokensPerDay    int64
	Note            string
	TTL             time.Duration
}

// User is who signed in to the dashboard (from Entra ID).
type User struct {
	ID    string   `json:"id"` // Entra object id (oid)
	Name  string   `json:"name"`
	Email string   `json:"email"`
	Roles []string `json:"roles"` // admin, viewer
}

// Session is a signed-in dashboard user.
type Session struct {
	User
	ExpiresAt time.Time `json:"expires_at"`
}

// AuditEntry records who did what.
type AuditEntry struct {
	ID      int64          `json:"id"`
	At      time.Time      `json:"at"`
	Actor   string         `json:"actor"`
	Action  string         `json:"action"` // token.create, token.revoke, token.rotate, login, login.denied
	Target  string         `json:"target"`
	Details map[string]any `json:"details"`
}

// Store is everything nautilm persists in the database.
type Store interface {
	CreateToken(ctx context.Context, n NewToken, actor string) (clear string, t Token, err error)
	ListTokens(ctx context.Context) ([]Token, error)
	GetToken(ctx context.Context, id int64) (Token, error)
	RevokeToken(ctx context.Context, id int64, actor string) (Token, error)
	// TokenByHash finds a token by the SHA-256 of its clear text (auth path).
	TokenByHash(ctx context.Context, hash string) (Token, error)
	TouchToken(ctx context.Context, id int64, at time.Time) error

	CreateSession(ctx context.Context, u User, ttl time.Duration) (cookie string, err error)
	GetSession(ctx context.Context, cookie string) (Session, error)
	DeleteSession(ctx context.Context, cookie string) error

	Audit(ctx context.Context, e AuditEntry) error
	ListAudit(ctx context.Context, limit int) ([]AuditEntry, error)

	Ping(ctx context.Context) error
	Close()
}

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
)

// TokenPrefix marks nautilm tokens: easy to spot in a leaked log or a secret scanner.
const TokenPrefix = "naut_"

// newSecret returns a clear-text token (or session id) and its hash.
func newSecret(prefix string) (clear, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS random source failing is not recoverable
	}
	clear = prefix + base64.RawURLEncoding.EncodeToString(b)
	return clear, Hash(clear)
}

// Hash is the SHA-256 (hex) of a clear-text secret: what the database keeps.
func Hash(clear string) string {
	s := sha256.Sum256([]byte(clear))
	return hex.EncodeToString(s[:])
}

func displayPrefix(clear string) string {
	if len(clear) > len(TokenPrefix)+6 {
		return clear[:len(TokenPrefix)+6]
	}
	return clear
}

// validate checks a NewToken before it is stored.
func (n NewToken) validate() error {
	if n.Agent == "" || len(n.Agent) > 128 {
		return errors.Join(ErrInvalid, errors.New("agent name is required (max 128 characters)"))
	}
	if len(n.Models) == 0 {
		return errors.Join(ErrInvalid, errors.New("at least one model alias is required"))
	}
	if n.TTL <= 0 || n.TTL > 366*24*time.Hour {
		return errors.Join(ErrInvalid, errors.New("validity must be between 1 day and 366 days"))
	}
	if n.TokensPerMinute < -1 || n.TokensPerDay < -1 {
		return errors.Join(ErrInvalid, errors.New("quotas must be -1 (no limit), 0 (default) or positive"))
	}
	return nil
}
