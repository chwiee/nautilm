package quota

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Valkey guarda os contadores no Valkey/ElastiCache: todas as réplicas do
// pedagio enxergam o mesmo total. INCRBY é atômico, sem corrida entre réplicas.
type Valkey struct{ c *redis.Client }

// NewValkey aceita redis://host:6379 ou rediss://host:6379 (TLS, ElastiCache).
func NewValkey(url string) (*Valkey, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	// Timeouts curtos: se o Valkey engasgar, a cota deixa passar (fail-open)
	// em vez de segurar a chamada do agente.
	opt.DialTimeout = 500 * time.Millisecond
	opt.ReadTimeout = 200 * time.Millisecond
	opt.WriteTimeout = 200 * time.Millisecond
	return &Valkey{c: redis.NewClient(opt)}, nil
}

func (v *Valkey) Ping(ctx context.Context) error { return v.c.Ping(ctx).Err() }

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
