// Package valkey opens the Valkey/ElastiCache connection shared by quotas
// and the usage ledger.
package valkey

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Connect accepts redis://host:6379 or rediss://host:6379 (TLS, ElastiCache).
//
// Short timeouts on purpose: if Valkey stalls, quotas let calls through
// (fail-open) and usage records are logged as lost, instead of holding the
// agent's LLM call.
func Connect(url string) (*redis.Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	opt.DialTimeout = 500 * time.Millisecond
	opt.ReadTimeout = 300 * time.Millisecond
	opt.WriteTimeout = 300 * time.Millisecond
	return redis.NewClient(opt), nil
}

// Ping checks the connection (used at startup to warn early).
func Ping(ctx context.Context, c *redis.Client) error { return c.Ping(ctx).Err() }
