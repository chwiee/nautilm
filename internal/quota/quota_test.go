package quota

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestMinutoEDia(t *testing.T) {
	ctx := context.Background()
	q := New(NewMemory())
	q.Now = fixed(time.Date(2026, 10, 22, 10, 30, 45, 0, time.UTC))
	l := Limits{PerMinute: 100, PerDay: 250}

	if d, _ := q.Check(ctx, "kubets", l); !d.Allowed || d.DayLeft != 250 {
		t.Fatalf("começo: %+v", d)
	}
	q.Spend(ctx, "kubets", l, 120) // passa um pouco (custo só é conhecido depois)
	d, _ := q.Check(ctx, "kubets", l)
	if d.Allowed || d.Window != "minuto" || d.RetryAfter != 15*time.Second {
		t.Fatalf("minuto estourado: %+v", d)
	}
	// Outro agente não é afetado.
	if d, _ := q.Check(ctx, "midts", l); !d.Allowed {
		t.Fatal("cota é por agente")
	}
	// Minuto seguinte: libera o minuto, mas o dia continua contando.
	q.Now = fixed(time.Date(2026, 10, 22, 10, 31, 1, 0, time.UTC))
	if d, _ := q.Check(ctx, "kubets", l); !d.Allowed || d.DayLeft != 130 {
		t.Fatalf("minuto novo: %+v", d)
	}
	q.Spend(ctx, "kubets", l, 90)
	q.Now = fixed(time.Date(2026, 10, 22, 10, 32, 0, 0, time.UTC))
	q.Spend(ctx, "kubets", l, 50)
	d, _ = q.Check(ctx, "kubets", l)
	if d.Allowed || d.Window != "dia" || d.RetryAfter != 13*time.Hour+28*time.Minute {
		t.Fatalf("dia estourado: %+v", d)
	}
}

func TestSemLimite(t *testing.T) {
	q := New(NewMemory())
	q.Spend(context.Background(), "x", Limits{}, 1_000_000)
	if d, _ := q.Check(context.Background(), "x", Limits{}); !d.Allowed || d.DayLeft != -1 {
		t.Fatalf("%+v", d)
	}
}

type broken struct{}

func (broken) Get(context.Context, string) (int64, error) { return 0, errors.New("valkey fora") }
func (broken) Add(context.Context, string, int64, time.Duration) (int64, error) {
	return 0, errors.New("valkey fora")
}

func TestFailOpen(t *testing.T) {
	q := New(broken{})
	d, err := q.Check(context.Background(), "x", Limits{PerMinute: 1, PerDay: 1})
	if err == nil || !d.Allowed {
		t.Fatalf("contador fora deve deixar passar E avisar: %+v %v", d, err)
	}
}

// Roda contra um Valkey de verdade se VALKEY_URL estiver definida
// (ex: VALKEY_URL=redis://localhost:6379 go test ./internal/quota).
func TestValkey(t *testing.T) {
	url := os.Getenv("VALKEY_URL")
	if url == "" {
		t.Skip("VALKEY_URL não definida")
	}
	ctx := context.Background()
	vk, err := NewValkey(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := vk.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	q := New(vk)
	q.Prefix = "pedagio:teste:" + time.Now().Format("150405.000") + ":"
	l := Limits{PerMinute: 100, PerDay: 1000}
	q.Spend(ctx, "a", l, 60)
	q.Spend(ctx, "a", l, 60) // duas "réplicas" somando no mesmo contador
	if d, _ := q.Check(ctx, "a", l); d.Allowed || d.Window != "minuto" {
		t.Fatalf("%+v", d)
	}
}
