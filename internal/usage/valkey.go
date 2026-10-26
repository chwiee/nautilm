package usage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Valkey stores the daily totals in Valkey/ElastiCache:
//
//	key:   nautilm:usage:<YYYY-MM-DD>          (one hash per day, UTC)
//	field: <agent>␟<caller>␟<team>␟<cost_center>␟<provider>␟<model>|<counter>
//	value: HINCRBY / HINCRBYFLOAT (atomic: replicas never race)
//
// Each day key expires after Retention, so there is no cleanup job.
//
// Trade-off: Valkey is fast and already in the stack, but it is not a
// forever database. If the node is lost without a snapshot, the totals of
// the affected days are lost too (the JSON log still has every call). If
// finance needs stronger guarantees, implement Store on DynamoDB.
type Valkey struct {
	c         redis.UniversalClient
	Prefix    string
	Retention time.Duration
}

// NewValkey uses an open connection (see internal/valkey.Connect).
func NewValkey(c redis.UniversalClient, retention time.Duration) *Valkey {
	return &Valkey{c: c, Prefix: "nautilm:usage:", Retention: retention}
}

const sep = "\x1f" // unit separator: never shows up in agent or model names

var counters = []string{"calls", "errors", "in", "out", "cache_read", "cache_write", "unpriced"}

func (v *Valkey) Add(ctx context.Context, r Record) error {
	row := toRow(r)
	k := v.Prefix + row.Day
	f := strings.Join([]string{row.Agent, row.Caller, row.Team, row.CostCenter, row.Provider, row.Model}, sep)
	vals := []int64{row.Calls, row.Errors, row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheWriteTokens, row.UnpricedCalls}
	pipe := v.c.TxPipeline()
	for i, name := range counters {
		if vals[i] != 0 {
			pipe.HIncrBy(ctx, k, f+"|"+name, vals[i])
		}
	}
	if row.CostUSD != 0 {
		pipe.HIncrByFloat(ctx, k, f+"|cost", row.CostUSD)
	}
	if v.Retention > 0 {
		pipe.Expire(ctx, k, v.Retention)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (v *Valkey) Rows(ctx context.Context, from, to time.Time) ([]Row, error) {
	var out []Row
	for d := from.UTC().Truncate(24 * time.Hour); !d.After(to.UTC()); d = d.Add(24 * time.Hour) {
		day := d.Format("2006-01-02")
		all, err := v.c.HGetAll(ctx, v.Prefix+day).Result()
		if err != nil {
			return nil, fmt.Errorf("usage: reading %s: %w", day, err)
		}
		byDim := map[Dim]*Totals{}
		for field, val := range all {
			dimPart, counter, ok := strings.Cut(field, "|")
			parts := strings.Split(dimPart, sep)
			if !ok || len(parts) != 6 {
				continue // unknown field: ignore instead of failing the report
			}
			dim := Dim{Day: day, Agent: parts[0], Caller: parts[1], Team: parts[2], CostCenter: parts[3], Provider: parts[4], Model: parts[5]}
			t, ok := byDim[dim]
			if !ok {
				t = &Totals{}
				byDim[dim] = t
			}
			if counter == "cost" {
				f, _ := strconv.ParseFloat(val, 64)
				t.CostUSD += f
				continue
			}
			n, _ := strconv.ParseInt(val, 10, 64)
			switch counter {
			case "calls":
				t.Calls += n
			case "errors":
				t.Errors += n
			case "in":
				t.InputTokens += n
			case "out":
				t.OutputTokens += n
			case "cache_read":
				t.CacheReadTokens += n
			case "cache_write":
				t.CacheWriteTokens += n
			case "unpriced":
				t.UnpricedCalls += n
			}
		}
		for d, t := range byDim {
			out = append(out, Row{Dim: d, Totals: *t})
		}
	}
	return out, nil
}
