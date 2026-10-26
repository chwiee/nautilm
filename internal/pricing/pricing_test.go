package pricing

import (
	"math"
	"strings"
	"testing"
	"time"
)

func day(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t.Add(12 * time.Hour) }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func catalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := New([]Entry{
		{Provider: "bedrock", Model: "sonnet", Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75, EffectiveFrom: "2026-01-01"},
		{Provider: "bedrock", Model: "sonnet", Input: 2, Output: 10, EffectiveFrom: "2026-10-15"}, // price cut
		{Provider: "anthropic-api", Model: "sonnet", Currency: "usd", Input: 3.5, Output: 16},
		{Provider: "ollama", Model: "*", Free: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCostUsesThePriceInEffectThatDay(t *testing.T) {
	c := catalog(t)
	tok := Tokens{Input: 1_000_000, Output: 100_000}
	before := c.Cost("bedrock", "sonnet", day("2026-10-14"), tok, nil)
	after := c.Cost("bedrock", "sonnet", day("2026-10-15"), tok, nil)
	if !near(before.CostUSD, 3+1.5) || before.Version != "bedrock/sonnet@2026-01-01" || before.Source != SourceTable {
		t.Errorf("before: %+v", before)
	}
	if !near(after.CostUSD, 2+1) || after.Version != "bedrock/sonnet@2026-10-15" {
		t.Errorf("after the price cut: %+v", after)
	}
	// Before the first entry: no price yet.
	if r := c.Cost("bedrock", "sonnet", day("2025-12-31"), tok, nil); r.Source != SourceUnpriced {
		t.Errorf("before any price: %+v", r)
	}
}

func TestSameModelDifferentProviderDifferentPrice(t *testing.T) {
	c := catalog(t)
	tok := Tokens{Input: 1_000_000}
	b := c.Cost("bedrock", "sonnet", day("2026-10-01"), tok, nil)
	a := c.Cost("anthropic-api", "sonnet", day("2026-10-01"), tok, nil)
	if !near(b.CostUSD, 3) || !near(a.CostUSD, 3.5) || a.Version != "anthropic-api/sonnet@initial" {
		t.Errorf("bedrock %+v, anthropic %+v", b, a)
	}
}

func TestCacheTokens(t *testing.T) {
	c := catalog(t)
	// 1M input of which 600k read from cache and 100k written; 0 output.
	r := c.Cost("bedrock", "sonnet", day("2026-10-01"), Tokens{Input: 1_000_000, CacheRead: 600_000, CacheWrite: 100_000}, nil)
	want := (300_000*3 + 600_000*0.30 + 100_000*3.75) / 1e6
	if !near(r.CostUSD, want) {
		t.Errorf("cache: %v, want %v", r.CostUSD, want)
	}
	// Without cache prices, cache tokens are charged as input (conservative).
	r = c.Cost("anthropic-api", "sonnet", day("2026-10-01"), Tokens{Input: 1_000_000, CacheRead: 500_000}, nil)
	if !near(r.CostUSD, 3.5) {
		t.Errorf("conservative cache: %v", r.CostUSD)
	}
}

func TestFreeUnpricedAndProviderReported(t *testing.T) {
	c := catalog(t)
	if r := c.Cost("ollama", "qwen2.5:7b", day("2026-10-01"), Tokens{Input: 5000}, nil); r.Source != SourceFree || r.CostUSD != 0 || r.Version != "ollama/*@initial" {
		t.Errorf("free: %+v", r)
	}
	if r := c.Cost("bedrock", "nova", day("2026-10-01"), Tokens{Input: 5000}, nil); r.Source != SourceUnpriced {
		t.Errorf("unknown model must be unpriced, not zero: %+v", r)
	}
	cost := 0.0123
	if r := c.Cost("bedrock", "nova", day("2026-10-01"), Tokens{Input: 5000}, &cost); r.Source != SourceProvider || r.CostUSD != cost {
		t.Errorf("provider-reported cost must win: %+v", r)
	}
	if got := c.Missing([][2]string{{"bedrock", "sonnet"}, {"bedrock", "nova"}, {"ollama", "x"}}, day("2026-10-01")); len(got) != 1 || got[0] != "bedrock/nova" {
		t.Errorf("missing: %v", got)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]Entry{
		"currency":        {Provider: "p", Model: "m", Input: 1, Currency: "BRL"},
		"negative":        {Provider: "p", Model: "m", Input: -1, Output: 1},
		"zero not free":   {Provider: "p", Model: "m"},
		"free with price": {Provider: "p", Model: "m", Free: true, Input: 1},
		"bad date":        {Provider: "p", Model: "m", Input: 1, EffectiveFrom: "22/10/2026"},
		"no model":        {Provider: "p", Input: 1},
	}
	for name, e := range cases {
		if _, err := New([]Entry{e}); err == nil {
			t.Errorf("%s: should fail", name)
		}
	}
	_, err := New([]Entry{{Provider: "p", Model: "m", Input: 1}, {Provider: "p", Model: "m", Input: 2}})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Errorf("duplicate: %v", err)
	}
}
