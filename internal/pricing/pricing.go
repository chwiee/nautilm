// Package pricing turns token usage into money.
//
// The price belongs to WHERE the call ran (provider) and WHICH model served
// it, not to the alias: "smart" may be Claude on Bedrock today and Claude on
// the Anthropic API tomorrow, and a fallback may be served by another model
// somewhere else. Prices are versioned by date, so a price change never
// rewrites history:
//
//	pricing:
//	  - provider: bedrock-us-east-2
//	    model: anthropic.claude-sonnet-5-5
//	    input: 3.00          # USD per 1M tokens
//	    output: 15.00
//	    cache_read: 0.30
//	    cache_write: 3.75
//	    effective_from: 2026-10-01
//	  - provider: ollama
//	    model: "*"           # every model of this provider
//	    free: true           # own infrastructure: zero on purpose
//
// A model without a price is NEVER counted as zero silently: its calls are
// tagged "unpriced" and show up as such in the dashboard.
package pricing

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Entry is one line of the price catalog. Prices are USD per 1M tokens.
type Entry struct {
	Provider      string  `yaml:"provider"`
	Model         string  `yaml:"model"` // "*" = any model of the provider
	Currency      string  `yaml:"currency"`
	Input         float64 `yaml:"input"`
	Output        float64 `yaml:"output"`
	CacheRead     float64 `yaml:"cache_read"`  // 0 = charged as input (conservative)
	CacheWrite    float64 `yaml:"cache_write"` // 0 = charged as input (conservative)
	Free          bool    `yaml:"free"`
	EffectiveFrom string  `yaml:"effective_from"` // YYYY-MM-DD (UTC); empty = always

	from time.Time
}

// Version identifies the exact price used (stored with every usage record).
func (e Entry) Version() string {
	d := e.EffectiveFrom
	if d == "" {
		d = "initial"
	}
	return e.Provider + "/" + e.Model + "@" + d
}

// Tokens of one call, in the normalized meaning (see openai.Usage):
// Input counts ALL input tokens, including cache reads and writes.
type Tokens struct {
	Input, Output, CacheRead, CacheWrite int
}

// Source tells where a cost came from.
const (
	SourceTable    = "table"    // price catalog
	SourceProvider = "provider" // reported by the provider in the response
	SourceFree     = "free"     // explicitly free (own infrastructure)
	SourceUnpriced = "unpriced" // no price: cost unknown, NOT zero
)

// Result is the cost of one call.
type Result struct {
	CostUSD float64
	Source  string
	Version string // which catalog entry was used ("" for provider/unpriced)
}

// Catalog holds the entries, indexed by provider+model, newest first.
type Catalog struct {
	byKey map[string][]Entry
}

func key(provider, model string) string { return provider + "\x00" + model }

// New validates the entries and builds the catalog.
func New(entries []Entry) (*Catalog, error) {
	c := &Catalog{byKey: map[string][]Entry{}}
	var errs []error
	seen := map[string]bool{}
	for i, e := range entries {
		where := fmt.Sprintf("pricing[%d] (%s/%s)", i, e.Provider, e.Model)
		if e.Provider == "" || e.Model == "" {
			errs = append(errs, fmt.Errorf("%s: provider and model are required", where))
			continue
		}
		if cur := strings.ToUpper(e.Currency); cur != "" && cur != "USD" {
			errs = append(errs, fmt.Errorf("%s: currency %q not supported; nautilm stores USD (that is how providers bill)", where, e.Currency))
		}
		if e.Input < 0 || e.Output < 0 || e.CacheRead < 0 || e.CacheWrite < 0 {
			errs = append(errs, fmt.Errorf("%s: negative price", where))
		}
		paid := e.Input > 0 || e.Output > 0 || e.CacheRead > 0 || e.CacheWrite > 0
		switch {
		case e.Free && paid:
			errs = append(errs, fmt.Errorf("%s: free: true cannot have prices", where))
		case !e.Free && !paid:
			// A forgotten price must not become "free" by accident.
			errs = append(errs, fmt.Errorf("%s: all prices are zero; set free: true if that is intended", where))
		}
		if e.EffectiveFrom != "" {
			t, err := time.Parse("2006-01-02", e.EffectiveFrom)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: effective_from %q must be YYYY-MM-DD", where, e.EffectiveFrom))
				continue
			}
			e.from = t
		}
		if seen[e.Version()] {
			errs = append(errs, fmt.Errorf("%s: duplicated entry for effective_from %q", where, e.EffectiveFrom))
		}
		seen[e.Version()] = true
		k := key(e.Provider, e.Model)
		c.byKey[k] = append(c.byKey[k], e)
	}
	for k := range c.byKey {
		sort.Slice(c.byKey[k], func(i, j int) bool { return c.byKey[k][i].from.After(c.byKey[k][j].from) })
	}
	return c, errors.Join(errs...)
}

// Lookup returns the entry in effect at "at" for provider+model, trying the
// exact model first and then the provider wildcard "*".
func (c *Catalog) Lookup(provider, model string, at time.Time) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	at = at.UTC()
	for _, m := range []string{model, "*"} {
		for _, e := range c.byKey[key(provider, m)] { // newest first
			if !e.from.After(at) {
				return e, true
			}
		}
	}
	return Entry{}, false
}

// Cost prices one call. A cost reported by the provider wins (it is what
// the provider will bill); otherwise the catalog is used.
func (c *Catalog) Cost(provider, model string, at time.Time, t Tokens, reported *float64) Result {
	if reported != nil && *reported >= 0 {
		return Result{CostUSD: *reported, Source: SourceProvider}
	}
	e, ok := c.Lookup(provider, model, at)
	if !ok {
		return Result{Source: SourceUnpriced}
	}
	if e.Free {
		return Result{Source: SourceFree, Version: e.Version()}
	}
	return Result{CostUSD: e.cost(t), Source: SourceTable, Version: e.Version()}
}

func (e Entry) cost(t Tokens) float64 {
	read, write := e.CacheRead, e.CacheWrite
	if read == 0 {
		read = e.Input
	}
	if write == 0 {
		write = e.Input
	}
	uncached := max(0, t.Input-t.CacheRead-t.CacheWrite)
	return (float64(uncached)*e.Input + float64(t.CacheRead)*read + float64(t.CacheWrite)*write +
		float64(t.Output)*e.Output) / 1e6
}

// Missing lists the provider/model pairs with no price in effect at "at".
func (c *Catalog) Missing(pairs [][2]string, at time.Time) []string {
	var out []string
	for _, p := range pairs {
		if _, ok := c.Lookup(p[0], p[1], at); !ok {
			out = append(out, p[0]+"/"+p[1])
		}
	}
	sort.Strings(out)
	return out
}
