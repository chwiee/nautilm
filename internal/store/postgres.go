package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Postgres is the production Store.
type Postgres struct {
	pool *pgxpool.Pool
	Now  func() time.Time
}

// OpenPostgres connects (postgres://user:pass@host:5432/db?sslmode=...) and
// applies pending migrations.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err) // the URL itself is never logged: it has the password
	}
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	p := &Postgres{pool: pool, Now: time.Now}
	if err := p.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrations: %w", err)
	}
	return p, nil
}

// migrate applies migrations/NNNN_*.sql in order, each in its own
// transaction. An advisory lock keeps two replicas starting together from
// running the same migration twice.
func (p *Postgres) migrate(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7238411)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", e.Name())
		}
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, v).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, _ := migrations.ReadFile("migrations/" + e.Name())
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

const tokenCols = `id, agent, token_prefix, team, cost_center, models, tokens_per_minute, tokens_per_day, note,
	created_by, created_at, expires_at, revoked_at, coalesce(revoked_by, ''), last_used_at`

func scanToken(row pgx.Row) (Token, error) {
	var t Token
	err := row.Scan(&t.ID, &t.Agent, &t.Prefix, &t.Team, &t.CostCenter, &t.Models, &t.TokensPerMinute, &t.TokensPerDay,
		&t.Note, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.RevokedAt, &t.RevokedBy, &t.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func (p *Postgres) CreateToken(ctx context.Context, n NewToken, actor string) (string, Token, error) {
	if err := n.validate(); err != nil {
		return "", Token{}, err
	}
	clear, hash := newSecret(TokenPrefix)
	expires := p.Now().UTC().Add(n.TTL)
	t, err := scanToken(p.pool.QueryRow(ctx, `INSERT INTO api_tokens
		(agent, token_hash, token_prefix, team, cost_center, models, tokens_per_minute, tokens_per_day, note, created_by, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+tokenCols,
		n.Agent, hash, displayPrefix(clear), n.Team, n.CostCenter, n.Models, n.TokensPerMinute, n.TokensPerDay, n.Note, actor, expires))
	if err != nil {
		return "", Token{}, err
	}
	return clear, t, nil
}

func (p *Postgres) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+tokenCols+` FROM api_tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *Postgres) GetToken(ctx context.Context, id int64) (Token, error) {
	return scanToken(p.pool.QueryRow(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE id = $1`, id))
}

func (p *Postgres) RevokeToken(ctx context.Context, id int64, actor string) (Token, error) {
	t, err := scanToken(p.pool.QueryRow(ctx, `UPDATE api_tokens SET revoked_at = coalesce(revoked_at, now()),
		revoked_by = coalesce(revoked_by, $2) WHERE id = $1 RETURNING `+tokenCols, id, actor))
	return t, err
}

func (p *Postgres) TokenByHash(ctx context.Context, hash string) (Token, error) {
	return scanToken(p.pool.QueryRow(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE token_hash = $1`, hash))
}

func (p *Postgres) TouchToken(ctx context.Context, id int64, at time.Time) error {
	_, err := p.pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = $2 WHERE id = $1`, id, at.UTC())
	return err
}

func (p *Postgres) CreateSession(ctx context.Context, u User, ttl time.Duration) (string, error) {
	clear, hash := newSecret("")
	// Expired sessions are cleaned on the way, no cron job needed.
	if _, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`); err != nil {
		return "", err
	}
	groups := u.Groups
	if groups == nil {
		groups = []string{}
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO sessions (id_hash, user_id, user_name, user_email, roles, groups, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, hash, u.ID, u.Name, u.Email, u.Roles, groups, p.Now().UTC().Add(ttl))
	return clear, err
}

func (p *Postgres) GetSession(ctx context.Context, cookie string) (Session, error) {
	var s Session
	err := p.pool.QueryRow(ctx, `SELECT user_id, user_name, user_email, roles, groups, expires_at FROM sessions
		WHERE id_hash = $1 AND expires_at > now()`, Hash(cookie)).Scan(&s.ID, &s.Name, &s.Email, &s.Roles, &s.Groups, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

func (p *Postgres) DeleteSession(ctx context.Context, cookie string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE id_hash = $1`, Hash(cookie))
	return err
}

func (p *Postgres) Audit(ctx context.Context, e AuditEntry) error {
	d, _ := json.Marshal(e.Details)
	if e.Details == nil {
		d = []byte("{}")
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO audit_log (actor, action, target, details) VALUES ($1,$2,$3,$4)`,
		e.Actor, e.Action, e.Target, d)
	return err
}

func (p *Postgres) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := p.pool.Query(ctx, `SELECT id, at, actor, action, target, details FROM audit_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var d []byte
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Target, &d); err != nil {
			return nil, err
		}
		json.Unmarshal(d, &e.Details)
		out = append(out, e)
	}
	return out, rows.Err()
}

const revisionCols = `id, kind, payload, actor, note, rollback_of, created_at`

func scanRevision(row pgx.Row) (Revision, error) {
	var r Revision
	var payload []byte
	err := row.Scan(&r.ID, &r.Kind, &payload, &r.Actor, &r.Note, &r.RollbackOf, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	r.Payload = payload
	return r, err
}

func (p *Postgres) SaveRevision(ctx context.Context, n NewRevision, actor string) (Revision, error) {
	if err := n.validate(); err != nil {
		return Revision{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Revision{}, err
	}
	defer tx.Rollback(ctx)
	// One writer per kind at a time (all replicas): the parent check below
	// cannot race with another save.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7238412, hashtext($1))`, n.Kind); err != nil {
		return Revision{}, err
	}
	var latest int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM config_revisions WHERE kind = $1`, n.Kind).Scan(&latest); err != nil {
		return Revision{}, err
	}
	if latest != n.Parent {
		return Revision{}, ErrConflict
	}
	var rb *int64
	if n.RollbackOf > 0 {
		rb = &n.RollbackOf
	}
	r, err := scanRevision(tx.QueryRow(ctx, `INSERT INTO config_revisions (kind, payload, actor, note, rollback_of)
		VALUES ($1,$2,$3,$4,$5) RETURNING `+revisionCols, n.Kind, []byte(n.Payload), actor, n.Note, rb))
	if err != nil {
		return Revision{}, err
	}
	return r, tx.Commit(ctx)
}

func (p *Postgres) LatestRevisions(ctx context.Context) (map[string]Revision, error) {
	rows, err := p.pool.Query(ctx, `SELECT DISTINCT ON (kind) `+revisionCols+` FROM config_revisions ORDER BY kind, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Revision{}
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out[r.Kind] = r
	}
	return out, rows.Err()
}

func (p *Postgres) ListRevisions(ctx context.Context, kind string, limit int) ([]Revision, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := p.pool.Query(ctx, `SELECT `+revisionCols+` FROM config_revisions WHERE kind = $1 ORDER BY id DESC LIMIT $2`, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) GetRevision(ctx context.Context, id int64) (Revision, error) {
	return scanRevision(p.pool.QueryRow(ctx, `SELECT `+revisionCols+` FROM config_revisions WHERE id = $1`, id))
}

func (p *Postgres) LastRevisionID(ctx context.Context) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM config_revisions`).Scan(&id)
	return id, err
}

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }
func (p *Postgres) Close()                         { p.pool.Close() }
