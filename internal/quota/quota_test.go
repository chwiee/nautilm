package quota

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/chwiee/nautilm/internal/valkey"
)

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestMinuteAndDay(t *testing.T) {
	ctx := context.Background()
	q := New(NewMemory())
	q.Now = fixed(time.Date(2026, 10, 22, 10, 30, 45, 0, time.UTC))
	l := Limits{PerMinute: 100, PerDay: 250}

	if d, _ := q.Check(ctx, "k8s", l); !d.Allowed || d.DayLeft != 250 {
		t.Fatalf("start: %+v", d)
	}
	q.Spend(ctx, "k8s", l, 120) // goes a bit over (cost is only known afterwards)
	d, _ := q.Check(ctx, "k8s", l)
	if d.Allowed || d.Window != "minute" || d.RetryAfter != 15*time.Second {
		t.Fatalf("minute exhausted: %+v", d)
	}
	// Another agent is not affected.
	if d, _ := q.Check(ctx, "mid", l); !d.Allowed {
		t.Fatal("quota is per agent")
	}
	// Next minute: the minute is free again, the day keeps counting.
	q.Now = fixed(time.Date(2026, 10, 22, 10, 31, 1, 0, time.UTC))
	if d, _ := q.Check(ctx, "k8s", l); !d.Allowed || d.DayLeft != 130 {
		t.Fatalf("new minute: %+v", d)
	}
	q.Spend(ctx, "k8s", l, 90)
	q.Now = fixed(time.Date(2026, 10, 22, 10, 32, 0, 0, time.UTC))
	q.Spend(ctx, "k8s", l, 50)
	d, _ = q.Check(ctx, "k8s", l)
	if d.Allowed || d.Window != "day" || d.RetryAfter != 13*time.Hour+28*time.Minute {
		t.Fatalf("day exhausted: %+v", d)
	}
}

func TestNoLimit(t *testing.T) {
	q := New(NewMemory())
	q.Spend(context.Background(), "x", Limits{}, 1_000_000)
	if d, _ := q.Check(context.Background(), "x", Limits{}); !d.Allowed || d.DayLeft != -1 {
		t.Fatalf("%+v", d)
	}
}

type broken struct{}

func (broken) Get(context.Context, string) (int64, error) { return 0, errors.New("valkey down") }
func (broken) Add(context.Context, string, int64, time.Duration) (int64, error) {
	return 0, errors.New("valkey down")
}

func TestFailOpen(t *testing.T) {
	q := New(broken{})
	d, err := q.Check(context.Background(), "x", Limits{PerMinute: 1, PerDay: 1})
	if err == nil || !d.Allowed {
		t.Fatalf("a broken counter must let the call through AND report it: %+v %v", d, err)
	}
}

// Runs against a real Valkey when VALKEY_URL is set
// (e.g. VALKEY_URL=redis://localhost:6379 go test ./internal/quota).
func TestValkey(t *testing.T) {
	url := os.Getenv("VALKEY_URL")
	if url == "" {
		t.Skip("VALKEY_URL not set")
	}
	ctx := context.Background()
	c, err := valkey.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := valkey.Ping(ctx, c); err != nil {
		t.Fatal(err)
	}
	q := New(NewValkey(c))
	q.Prefix = "nautilm:test:" + time.Now().Format("150405.000") + ":"
	l := Limits{PerMinute: 100, PerDay: 1000}
	q.Spend(ctx, "a", l, 60)
	q.Spend(ctx, "a", l, 60) // two "replicas" adding to the same counter
	if d, _ := q.Check(ctx, "a", l); d.Allowed || d.Window != "minute" {
		t.Fatalf("%+v", d)
	}
}
