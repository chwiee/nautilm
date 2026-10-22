// Package quota limita quantos tokens cada agente gasta por minuto e por dia.
//
// Por que? Um agente em loop (bug de tool, prompt que nunca termina) pode
// queimar o orçamento do mês numa noite. Com a cota, ele leva 429 e os
// outros agentes seguem funcionando.
//
// Como funciona (janela fixa, simples de explicar):
//
//	antes da chamada:  usado no minuto >= limite? → 429 + Retry-After
//	                   usado no dia    >= limite? → 429 + Retry-After
//	depois da chamada: soma tokens de entrada + saída nos dois contadores
//
// Só sabemos o custo DEPOIS da resposta, então a última chamada pode passar
// um pouco do limite. É o mesmo trade-off dos gateways prontos: estimar
// antes custaria um tokenizer por modelo.
//
// Contadores: em memória (uma réplica / dev) ou no Valkey (várias réplicas
// enxergam o mesmo número).
package quota

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Limits de um agente. 0 = sem limite naquela janela.
type Limits struct {
	PerMinute int64
	PerDay    int64
}

// Counter guarda os contadores. Add soma e devolve o novo total.
type Counter interface {
	Get(ctx context.Context, key string) (int64, error)
	Add(ctx context.Context, key string, n int64, ttl time.Duration) (int64, error)
}

// Decision diz se o pedido pode seguir.
type Decision struct {
	Allowed    bool
	Window     string        // "minuto" | "dia" (qual estourou)
	RetryAfter time.Duration // quanto falta para a janela virar
	DayLeft    int64         // tokens restantes no dia (-1 = sem limite)
}

// Quota aplica limites sobre um Counter.
type Quota struct {
	Counter Counter
	Prefix  string
	Now     func() time.Time // trocável nos testes
}

func New(c Counter) *Quota {
	return &Quota{Counter: c, Prefix: "pedagio:cota:", Now: time.Now}
}

// Check confere se o agente ainda tem cota. Erro do contador = deixa passar
// (fail-open): cota protege custo, não pode derrubar os agentes. Quem chama
// loga o erro.
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
			return Decision{Window: "dia", RetryAfter: untilNextDay(now), DayLeft: 0}, nil
		}
	}
	if l.PerMinute > 0 {
		used, err := q.Counter.Get(ctx, minKey)
		if err != nil {
			return d, err
		}
		if used >= l.PerMinute {
			return Decision{Window: "minuto", RetryAfter: untilNextMinute(now), DayLeft: d.DayLeft}, nil
		}
	}
	return d, nil
}

// Spend soma os tokens gastos. TTL um pouco maior que a janela: a chave
// some sozinha, sem job de limpeza.
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

// Memory é o contador em memória (uma réplica só).
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
	// limpeza preguiçosa: aproveita a escrita para jogar fora o que expirou
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
