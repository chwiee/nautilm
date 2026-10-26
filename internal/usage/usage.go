// Package usage is the ledger: who used which model, where, how many tokens
// and how much it cost. It is what answers "last week, k8s used 1M tokens =
// US$ X" and splits the bill between agents and teams.
//
// Why not Prometheus? Prometheus is for operations: ~15 days of retention,
// approximations when a scrape is lost, counters that reset. Splitting cost
// needs months of numbers that add up. So every call is added to a DAILY
// total per dimension (day × agent × caller × team × cost center × provider
// × model), stored in Valkey with a retention period.
//
// The detail of each call stays in the JSON log (audit trail).
package usage

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/chwiee/nautilm/internal/pricing"
)

// Record is one call to a model (one fallback attempt = one record).
type Record struct {
	Time time.Time
	// Agent is who PAYS. Usually the caller; with on-behalf-of (e.g. the
	// orchestrator routing for k8s), the agent served.
	Agent string
	// Caller is the authenticated identity that made the call.
	Caller     string
	Team       string
	CostCenter string
	Kind       string // chat | embedding
	Alias      string // what the agent asked for
	Provider   string // where it ran
	Model      string // real model that served it
	Status     int
	Tokens     pricing.Tokens
	Cost       pricing.Result
	TraceID    string
	Duration   time.Duration
}

// Dim is one row of the daily total.
type Dim struct {
	Day        string `json:"day"` // YYYY-MM-DD (UTC)
	Agent      string `json:"agent"`
	Caller     string `json:"caller"` // empty when the agent called directly
	Team       string `json:"team"`
	CostCenter string `json:"cost_center"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
}

// Totals are the summed counters.
type Totals struct {
	Calls            int64   `json:"calls"`
	Errors           int64   `json:"errors"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// UnpricedCalls: calls whose model had no price. Their cost is UNKNOWN,
	// not zero; the dashboard warns about them.
	UnpricedCalls int64 `json:"unpriced_calls"`
}

func (t *Totals) add(o Totals) {
	t.Calls += o.Calls
	t.Errors += o.Errors
	t.InputTokens += o.InputTokens
	t.OutputTokens += o.OutputTokens
	t.CacheReadTokens += o.CacheReadTokens
	t.CacheWriteTokens += o.CacheWriteTokens
	t.CostUSD += o.CostUSD
	t.UnpricedCalls += o.UnpricedCalls
}

// Row is a dimension with its totals.
type Row struct {
	Dim
	Totals
}

// Store keeps the daily totals.
type Store interface {
	Add(ctx context.Context, r Record) error
	// Rows returns every row from day "from" to day "to", inclusive (UTC).
	Rows(ctx context.Context, from, to time.Time) ([]Row, error)
}

// toRow turns a record into its dimension and increments.
func toRow(r Record) Row {
	caller := r.Caller
	if caller == r.Agent {
		caller = ""
	}
	t := Totals{Calls: 1}
	if r.Status >= 400 || r.Status == 0 {
		t.Errors = 1
	}
	t.InputTokens = int64(r.Tokens.Input)
	t.OutputTokens = int64(r.Tokens.Output)
	t.CacheReadTokens = int64(r.Tokens.CacheRead)
	t.CacheWriteTokens = int64(r.Tokens.CacheWrite)
	t.CostUSD = r.Cost.CostUSD
	// A failed call with no tokens costs nothing: it is not "unpriced".
	if r.Cost.Source == pricing.SourceUnpriced && (t.InputTokens > 0 || t.OutputTokens > 0) {
		t.UnpricedCalls = 1
	}
	return Row{Dim: Dim{
		Day: r.Time.UTC().Format("2006-01-02"), Agent: r.Agent, Caller: caller, Team: r.Team,
		CostCenter: r.CostCenter, Provider: r.Provider, Model: r.Model,
	}, Totals: t}
}

// Memory is the in-process store (single replica, dev and tests). It keeps
// everything until the process stops.
type Memory struct {
	mu   sync.Mutex
	rows map[Dim]*Totals
}

func NewMemory() *Memory { return &Memory{rows: map[Dim]*Totals{}} }

func (m *Memory) Add(_ context.Context, r Record) error {
	row := toRow(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.rows[row.Dim]
	if !ok {
		t = &Totals{}
		m.rows[row.Dim] = t
	}
	t.add(row.Totals)
	return nil
}

func (m *Memory) Rows(_ context.Context, from, to time.Time) ([]Row, error) {
	f, t := from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02")
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Row
	for d, tot := range m.rows {
		if d.Day >= f && d.Day <= t {
			out = append(out, Row{Dim: d, Totals: *tot})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out, nil
}
