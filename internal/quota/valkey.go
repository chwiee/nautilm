package quota

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Valkey keeps the counters in Valkey/ElastiCache: every nautilm replica
// sees the same total. INCRBY is atomic, so replicas do not race.
type Valkey struct{ c redis.UniversalClient }

// NewValkey uses an open connection (see internal/valkey.Connect).
func NewValkey(c redis.UniversalClient) *Valkey { return &Valkey{c: c} }

func (v *Valkey) Get(ctx context.Context, key string) (int64, error) {
	n, err := v.c.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}

func (v *Valkey) Add(ctx context.Context, key string, n int64, ttl time.Duration) (int64, error) {
	pipe := v.c.TxPipeline()
	incr := pipe.IncrBy(ctx, key, n)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}
