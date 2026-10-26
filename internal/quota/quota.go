// Package quota limits how many tokens each agent spends per minute and per day.
//
// Why? An agent stuck in a loop (tool bug, prompt that never ends) can burn
// the month's budget overnight. With a quota it gets a 429 and the other
// agents keep working.
//
// How it works (fixed windows, easy to explain):
//
//	before the call: used this minute >= limit? → 429 + Retry-After
//	                 used today       >= limit? → 429 + Retry-After
//	after the call:  add input + output tokens to both counters
//
// The cost is only known AFTER the response, so the last call may go a bit
// over the limit. It is the same trade-off as off-the-shelf gateways:
// estimating beforehand would need one tokenizer per model.
//
// Counters: in memory (single replica / dev) or in Valkey (every replica
// sees the same number).
package quota

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Limits of one agent. 0 = no limit in that window.
type Limits struct {
	PerMinute int64
	PerDay    int64
}

// Counter stores the counters. Add increments and returns the new total.
type Counter interface {
	Get(ctx context.Context, key string) (int64, error)
	Add(ctx context.Context, key string, n int64, ttl time.Duration) (int64, error)
}

// Decision tells whether the request may go on.
type Decision struct {
	Allowed    bool
	Window     string        // "minute" | "day" (which one is exhausted)
	RetryAfter time.Duration // time until the window rolls over
	DayLeft    int64         // tokens left today (-1 = no limit)
}

// Quota applies limits on top of a Counter.
type Quota struct {
	Counter Counter
	Prefix  string
	Now     func() time.Time // swappable in tests
}

func New(c Counter) *Quota {
	return &Quota{Counter: c, Prefix: "nautilm:quota:", Now: time.Now}
}

// Check tells whether the agent still has quota. A counter error lets the
// call through (fail-open): quotas protect cost, they must not take the
// agents down. The caller logs the error.
func (q *Quota) Check(ctx context.Context, agent string, l Limits) (Decision, error) {
	now := q.Now().UTC()
	d := Decision{Allowed: true, DayLeft: -1}
	minKey, dayKey := q.keys(agent, now)

	if l.PerDay > 0 {
		used, err := q.Counter.Get(ctx, dayKey)
		if err != nil {
			return d, err
		}
		d.DayLeft = max(0, l.PerDay-used)
		if used >= l.PerDay {
			return Decision{Window: "day", RetryAfter: untilNextDay(now), DayLeft: 0}, nil
		}
	}
	if l.PerMinute > 0 {
		used, err := q.Counter.Get(ctx, minKey)
		if err != nil {
			return d, err
		}
		if used >= l.PerMinute {
			return Decision{Window: "minute", RetryAfter: untilNextMinute(now), DayLeft: d.DayLeft}, nil
		}
	}
	return d, nil
}

// Spend adds the tokens spent. The TTL is a bit longer than the window: the
// key disappears by itself, no cleanup job.
func (q *Quota) Spend(ctx context.Context, agent string, l Limits, tokens int64) error {
	if tokens <= 0 {
		return nil
	}
	now := q.Now().UTC()
	minKey, dayKey := q.keys(agent, now)
	if l.PerMinute > 0 {
		if _, err := q.Counter.Add(ctx, minKey, tokens, 2*time.Minute); err != nil {
			return err
		}
	}
	if l.PerDay > 0 {
		if _, err := q.Counter.Add(ctx, dayKey, tokens, 25*time.Hour); err != nil {
			return err
		}
	}
	return nil
}

func (q *Quota) keys(agent string, now time.Time) (minute, day string) {
	return fmt.Sprintf("%s%s:m:%s", q.Prefix, agent, now.Format("200601021504")),
		fmt.Sprintf("%s%s:d:%s", q.Prefix, agent, now.Format("20060102"))
}

func untilNextMinute(now time.Time) time.Duration {
	return now.Truncate(time.Minute).Add(time.Minute).Sub(now)
}

func untilNextDay(now time.Time) time.Duration {
	y, m, d := now.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC).Sub(now)
}

// Memory is the in-memory counter (single replica).
type Memory struct {
	mu   sync.Mutex
	vals map[string]memVal
}

type memVal struct {
	n   int64
	exp time.Time
}

func NewMemory() *Memory { return &Memory{vals: map[string]memVal{}} }

func (m *Memory) Get(_ context.Context, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[key]
	if !ok || time.Now().After(v.exp) {
		return 0, nil
	}
	return v.n, nil
}

func (m *Memory) Add(_ context.Context, key string, n int64, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	// lazy cleanup: use the write to drop what expired
	if len(m.vals) > 10_000 {
		for k, v := range m.vals {
			if now.After(v.exp) {
				delete(m.vals, k)
			}
		}
	}
	v := m.vals[key]
	if now.After(v.exp) {
		v = memVal{}
	}
	v.n += n
	v.exp = now.Add(ttl)
	m.vals[key] = v
	return v.n, nil
}
