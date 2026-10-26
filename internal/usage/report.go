package usage

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Dimensions a report can be grouped by.
var Dimensions = []string{"agent", "team", "cost_center", "provider", "model", "caller", "day"}

// Query asks for a report: days From..To (inclusive, UTC) grouped by GroupBy.
type Query struct {
	From, To time.Time
	GroupBy  []string
}

// Group is one line of the report.
type Group struct {
	Key map[string]string `json:"key"`
	Totals
	// CostShare is this group's share of the period cost (0..1).
	CostShare float64 `json:"cost_share"`
	// TokenShare is this group's share of the period tokens (0..1). Useful
	// when everything is free (local models) and cost shares are all zero.
	TokenShare float64 `json:"token_share"`
	// Previous is the same group in the previous period (same length).
	Previous Totals `json:"previous"`
}

// Report is the answer of /v1/usage.
type Report struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Days     int      `json:"days"`
	GroupBy  []string `json:"group_by"`
	Currency string   `json:"currency"`
	Total    Totals   `json:"total"`
	Previous Totals   `json:"previous_total"`
	Groups   []Group  `json:"groups"`
	// Note reminds that this is an ESTIMATE (price table × tokens).
	Note string `json:"note"`
}

// ParseQuery validates the query string parameters (from, to, group_by).
// Defaults: the last 7 days (today included), grouped by agent.
func ParseQuery(from, to, groupBy string, now time.Time) (Query, error) {
	q := Query{To: now.UTC(), GroupBy: []string{"agent"}}
	q.From = q.To.AddDate(0, 0, -6)
	var err error
	if from != "" {
		if q.From, err = time.Parse("2006-01-02", from); err != nil {
			return q, fmt.Errorf("from must be YYYY-MM-DD")
		}
	}
	if to != "" {
		if q.To, err = time.Parse("2006-01-02", to); err != nil {
			return q, fmt.Errorf("to must be YYYY-MM-DD")
		}
	}
	q.From, q.To = truncDay(q.From), truncDay(q.To)
	if q.To.Before(q.From) {
		return q, fmt.Errorf("to is before from")
	}
	if days(q) > 400 {
		return q, fmt.Errorf("period too long (max 400 days)")
	}
	if groupBy != "" {
		q.GroupBy = nil
		for _, g := range strings.Split(groupBy, ",") {
			g = strings.TrimSpace(g)
			if !slices.Contains(Dimensions, g) {
				return q, fmt.Errorf("group_by %q is not valid (use %s)", g, strings.Join(Dimensions, ", "))
			}
			if !slices.Contains(q.GroupBy, g) {
				q.GroupBy = append(q.GroupBy, g)
			}
		}
	}
	return q, nil
}

func truncDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func days(q Query) int { return int(q.To.Sub(q.From).Hours()/24) + 1 }

func dimValue(d Dim, name string) string {
	switch name {
	case "agent":
		return d.Agent
	case "team":
		return d.Team
	case "cost_center":
		return d.CostCenter
	case "provider":
		return d.Provider
	case "model":
		return d.Model
	case "caller":
		return d.Caller
	case "day":
		return d.Day
	}
	return ""
}

func groupKey(d Dim, by []string) string {
	parts := make([]string, len(by))
	for i, b := range by {
		parts[i] = dimValue(d, b)
	}
	return strings.Join(parts, sep)
}

// Build runs the query: the period, the previous period of the same length
// (for the "vs previous" column) and the shares.
func Build(ctx context.Context, s Store, q Query) (Report, error) {
	n := days(q)
	rows, err := s.Rows(ctx, q.From, q.To)
	if err != nil {
		return Report{}, err
	}
	prevTo := q.From.AddDate(0, 0, -1)
	prevRows, err := s.Rows(ctx, prevTo.AddDate(0, 0, -(n-1)), prevTo)
	if err != nil {
		return Report{}, err
	}

	rep := Report{From: q.From.Format("2006-01-02"), To: q.To.Format("2006-01-02"), Days: n, GroupBy: q.GroupBy,
		Currency: "USD", Groups: []Group{},
		Note: "Estimate: tokens reported by each provider × the nautilm price table (or the cost the provider reported). Reconcile monthly with the provider invoices."}
	groups := map[string]*Group{}
	for _, r := range rows {
		k := groupKey(r.Dim, q.GroupBy)
		g, ok := groups[k]
		if !ok {
			g = &Group{Key: map[string]string{}}
			for _, b := range q.GroupBy {
				g.Key[b] = dimValue(r.Dim, b)
			}
			groups[k] = g
		}
		g.add(r.Totals)
		rep.Total.add(r.Totals)
	}
	for _, r := range prevRows {
		rep.Previous.add(r.Totals)
		if g, ok := groups[groupKey(r.Dim, q.GroupBy)]; ok && !slices.Contains(q.GroupBy, "day") {
			g.Previous.add(r.Totals)
		}
	}
	totalTokens := float64(rep.Total.InputTokens + rep.Total.OutputTokens)
	for _, g := range groups {
		if rep.Total.CostUSD > 0 {
			g.CostShare = g.CostUSD / rep.Total.CostUSD
		}
		if totalTokens > 0 {
			g.TokenShare = float64(g.InputTokens+g.OutputTokens) / totalTokens
		}
		rep.Groups = append(rep.Groups, *g)
	}
	sort.Slice(rep.Groups, func(i, j int) bool {
		a, b := rep.Groups[i], rep.Groups[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		if ta, tb := a.InputTokens+a.OutputTokens, b.InputTokens+b.OutputTokens; ta != tb {
			return ta > tb
		}
		return groupKeyOf(a, q.GroupBy) < groupKeyOf(b, q.GroupBy)
	})
	return rep, nil
}

func groupKeyOf(g Group, by []string) string {
	parts := make([]string, len(by))
	for i, b := range by {
		parts[i] = g.Key[b]
	}
	return strings.Join(parts, sep)
}

// WriteCSV writes the report for finance (one line per group).
func WriteCSV(w io.Writer, rep Report) error {
	cw := csv.NewWriter(w)
	header := append(slices.Clone(rep.GroupBy), "calls", "errors", "input_tokens", "output_tokens",
		"cache_read_tokens", "cache_write_tokens", "cost_usd", "cost_share_pct", "unpriced_calls",
		"previous_cost_usd", "period_from", "period_to")
	if err := cw.Write(header); err != nil {
		return err
	}
	i64 := func(n int64) string { return strconv.FormatInt(n, 10) }
	for _, g := range rep.Groups {
		var line []string
		for _, b := range rep.GroupBy {
			line = append(line, g.Key[b])
		}
		line = append(line, i64(g.Calls), i64(g.Errors), i64(g.InputTokens), i64(g.OutputTokens),
			i64(g.CacheReadTokens), i64(g.CacheWriteTokens), strconv.FormatFloat(g.CostUSD, 'f', 6, 64),
			strconv.FormatFloat(g.CostShare*100, 'f', 2, 64), i64(g.UnpricedCalls),
			strconv.FormatFloat(g.Previous.CostUSD, 'f', 6, 64), rep.From, rep.To)
		if err := cw.Write(line); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
