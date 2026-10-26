package usage

import (
	"bytes"
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chwiee/nautilm/internal/pricing"
	"github.com/chwiee/nautilm/internal/valkey"
)

func at(day string, hour int) time.Time {
	t, _ := time.Parse("2006-01-02", day)
	return t.Add(time.Duration(hour) * time.Hour)
}

func rec(day, agent, caller, provider, model string, in, out int, cost float64, src string) Record {
	return Record{Time: at(day, 10), Agent: agent, Caller: caller, Team: map[string]string{"k8s": "ce", "mid": "ce", "marcia": "fin"}[agent],
		Provider: provider, Model: model, Kind: "chat", Status: 200,
		Tokens: pricing.Tokens{Input: in, Output: out}, Cost: pricing.Result{CostUSD: cost, Source: src}}
}

// scenario: last week (19..25) and the week before (12..18).
func seed(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	for _, r := range []Record{
		rec("2026-10-20", "k8s", "k8s", "bedrock", "sonnet", 800_000, 200_000, 5.40, pricing.SourceTable),
		rec("2026-10-21", "k8s", "k8s", "bedrock", "sonnet", 100_000, 0, 0.30, pricing.SourceTable),
		rec("2026-10-22", "mid", "mid", "anthropic-api", "haiku", 3_500_000, 500_000, 6.00, pricing.SourceTable),
		// the orchestrator routing on behalf of k8s: billed to k8s, caller maestro
		rec("2026-10-22", "k8s", "maestro", "bedrock", "nova", 10_000, 1_000, 0.01, pricing.SourceTable),
		rec("2026-10-23", "marcia", "marcia", "ollama", "qwen", 50_000, 5_000, 0, pricing.SourceFree),
		rec("2026-10-24", "claudia", "claudia", "bedrock", "mystery", 1_000, 100, 0, pricing.SourceUnpriced),
		rec("2026-10-14", "k8s", "k8s", "bedrock", "sonnet", 100_000, 10_000, 0.45, pricing.SourceTable), // previous week
		{Time: at("2026-10-25", 1), Agent: "mid", Caller: "mid", Provider: "anthropic-api", Model: "haiku", Status: 503,
			Cost: pricing.Result{Source: pricing.SourceUnpriced}}, // failed attempt: an error, not unpriced
	} {
		if err := s.Add(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func checkReport(t *testing.T, s Store) {
	t.Helper()
	q, err := ParseQuery("2026-10-19", "2026-10-25", "agent", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Build(context.Background(), s, q)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Days != 7 || !near(rep.Total.CostUSD, 11.71) || rep.Total.Calls != 7 || rep.Total.Errors != 1 || rep.Total.UnpricedCalls != 1 {
		t.Fatalf("total: %+v", rep)
	}
	if !near(rep.Previous.CostUSD, 0.45) {
		t.Errorf("previous period: %+v", rep.Previous)
	}
	got := map[string]Group{}
	var order []string
	for _, g := range rep.Groups {
		got[g.Key["agent"]] = g
		order = append(order, g.Key["agent"])
	}
	// Sorted by cost: mid 6.00, k8s 5.71, then by tokens: marcia, claudia.
	if strings.Join(order, ",") != "mid,k8s,marcia,claudia" {
		t.Errorf("order: %v", order)
	}
	k8s := got["k8s"]
	if k8s.InputTokens != 910_000 || !near(k8s.CostUSD, 5.71) || !near(k8s.Previous.CostUSD, 0.45) || !near(k8s.CostShare, 5.71/11.71) {
		t.Errorf("k8s (with the maestro call billed to it): %+v", k8s)
	}
	if got["claudia"].UnpricedCalls != 1 {
		t.Errorf("claudia must show the unpriced call: %+v", got["claudia"])
	}

	// By provider and model: the "where" view.
	q.GroupBy = []string{"provider", "model"}
	rep, _ = Build(context.Background(), s, q)
	if len(rep.Groups) != 5 || rep.Groups[0].Key["provider"] != "anthropic-api" {
		t.Errorf("by provider/model: %+v", rep.Groups)
	}
	// By caller: what the orchestrator spent on behalf of others.
	q.GroupBy = []string{"agent", "caller"}
	rep, _ = Build(context.Background(), s, q)
	found := false
	for _, g := range rep.Groups {
		if g.Key["agent"] == "k8s" && g.Key["caller"] == "maestro" && g.Calls == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("on-behalf-of row missing: %+v", rep.Groups)
	}

	// CSV for finance.
	q.GroupBy = []string{"team"}
	rep, _ = Build(context.Background(), s, q)
	var buf bytes.Buffer
	if err := WriteCSV(&buf, rep); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.HasPrefix(lines[0], "team,calls,errors,input_tokens") || !strings.HasPrefix(lines[1], "ce,4,0,4410000,701000") || !strings.HasPrefix(lines[3], ",2,1,") {
		t.Errorf("csv:\n%s", buf.String())
	}
}

func TestMemory(t *testing.T) {
	s := NewMemory()
	seed(t, s)
	checkReport(t, s)
}

// Same scenario against a real Valkey (VALKEY_URL=redis://localhost:6379).
func TestValkey(t *testing.T) {
	url := os.Getenv("VALKEY_URL")
	if url == "" {
		t.Skip("VALKEY_URL not set")
	}
	c, err := valkey.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	s := NewValkey(c, time.Hour)
	s.Prefix = "nautilm:test:" + time.Now().Format("150405.000") + ":usage:"
	seed(t, s)
	checkReport(t, s)
	if ttl := c.TTL(context.Background(), s.Prefix+"2026-10-20").Val(); ttl <= 0 || ttl > time.Hour {
		t.Errorf("day keys must expire with the retention: %v", ttl)
	}
}

func TestParseQuery(t *testing.T) {
	now := at("2026-10-26", 15)
	q, err := ParseQuery("", "", "", now)
	if err != nil || q.From.Format("2006-01-02") != "2026-10-20" || q.To.Format("2006-01-02") != "2026-10-26" || q.GroupBy[0] != "agent" {
		t.Errorf("defaults: %+v %v", q, err)
	}
	for _, bad := range [][3]string{{"2026-10-26", "2026-10-01", ""}, {"26/10", "", ""}, {"", "", "agent,color"}, {"2024-01-01", "2026-10-01", ""}} {
		if _, err := ParseQuery(bad[0], bad[1], bad[2], now); err == nil {
			t.Errorf("should fail: %v", bad)
		}
	}
}
