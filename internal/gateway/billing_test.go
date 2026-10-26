package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/internal/usage"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// usageReport reads /v1/usage with the dashboard token.
func usageReport(t *testing.T, env testEnv, groupBy string) usage.Report {
	t.Helper()
	resp := do(t, http.MethodGet, env.srv.URL+"/v1/usage?group_by="+groupBy, "dash-secret", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("usage: %d %s", resp.StatusCode, b)
	}
	var rep usage.Report
	json.NewDecoder(resp.Body).Decode(&rep)
	return rep
}

func group(rep usage.Report, kv ...string) (usage.Group, bool) {
	for _, g := range rep.Groups {
		match := true
		for i := 0; i+1 < len(kv); i += 2 {
			if g.Key[kv[i]] != kv[i+1] {
				match = false
			}
		}
		if match {
			return g, true
		}
	}
	return usage.Group{}, false
}

func TestCostHeadersAndLedger(t *testing.T) {
	env := setup(t, &fake{}, &fake{})
	resp := post(t, env.srv, "tok-k8s", fmt.Sprintf(hello, "smart"))
	// (10 × 3 + 3 × 15) / 1M = 0.000075
	if resp.Header.Get("X-Nautilm-Cost-USD") != "0.00007500" || resp.Header.Get("X-Nautilm-Price-Source") != "table" ||
		resp.Header.Get("X-Nautilm-Billed-To") != "k8s" {
		t.Fatalf("headers: %v", resp.Header)
	}
	post(t, env.srv, "tok-mid", fmt.Sprintf(hello, "fast")) // (10×0.5 + 3×1)/1M = 0.000008

	rep := usageReport(t, env, "agent")
	k8s, _ := group(rep, "agent", "k8s")
	mid, _ := group(rep, "agent", "mid")
	if !near(k8s.CostUSD, 0.000075) || k8s.InputTokens != 10 || k8s.OutputTokens != 3 || k8s.Calls != 1 {
		t.Errorf("k8s: %+v", k8s)
	}
	if !near(mid.CostUSD, 0.000008) || !near(rep.Total.CostUSD, 0.000083) || rep.Groups[0].Key["agent"] != "k8s" {
		t.Errorf("mid/total/order: %+v", rep)
	}
	team := usageReport(t, env, "team,cost_center")
	if g, ok := group(team, "team", "ce", "cost_center", "CC-1"); !ok || g.Calls != 2 {
		t.Errorf("k8s and mid share team ce / CC-1: %+v", team.Groups)
	}
	if !strings.Contains(metricsText(env.metrics), `nautilm_cost_usd_total{agent="k8s",provider="a",model="big-model",source="table"} 7.5e-05`) {
		t.Errorf("cost metric:\n%s", metricsText(env.metrics))
	}
}

// The fallback is billed to WHO SERVED (provider b, small-model), and the
// failed attempt on provider a is recorded as an error with no cost.
func TestFallbackIsBilledToWhoServed(t *testing.T) {
	env := setup(t, &fake{err: provider.Errorf(503, "x", "down")}, &fake{})
	resp := post(t, env.srv, "tok-k8s", fmt.Sprintf(hello, "smart"))
	if resp.Header.Get("X-Nautilm-Cost-USD") != "0.00000800" {
		t.Fatalf("cost must use the price of small-model: %v", resp.Header)
	}
	rep := usageReport(t, env, "provider,model")
	a, _ := group(rep, "provider", "a", "model", "big-model")
	b, _ := group(rep, "provider", "b", "model", "small-model")
	if a.Calls != 1 || a.Errors != 1 || a.CostUSD != 0 || b.Calls != 1 || !near(b.CostUSD, 0.000008) {
		t.Errorf("a=%+v b=%+v", a, b)
	}
}

func TestOnBehalfOf(t *testing.T) {
	env := setup(t, &fake{}, &fake{})
	// maestro is allowed: the call is billed to k8s (team ce), caller maestro.
	resp := post(t, env.srv, "tok-maestro", fmt.Sprintf(hello, "fast"), HeaderOnBehalfOf, "k8s")
	if resp.Header.Get("X-Nautilm-Billed-To") != "k8s" {
		t.Fatalf("maestro on behalf of k8s: %v", resp.Header)
	}
	// k8s is NOT allowed: the header is ignored, k8s pays its own call.
	resp = post(t, env.srv, "tok-k8s", fmt.Sprintf(hello, "fast"), HeaderOnBehalfOf, "mid")
	if resp.Header.Get("X-Nautilm-Billed-To") != "k8s" {
		t.Fatalf("k8s must not bill mid: %v", resp.Header)
	}
	rep := usageReport(t, env, "agent,caller,team")
	if g, ok := group(rep, "agent", "k8s", "caller", "maestro", "team", "ce"); !ok || g.Calls != 1 {
		t.Errorf("on-behalf row: %+v", rep.Groups)
	}
	if g, ok := group(rep, "agent", "k8s", "caller", ""); !ok || g.Calls != 1 {
		t.Errorf("k8s own call: %+v", rep.Groups)
	}
	if _, ok := group(rep, "agent", "mid"); ok {
		t.Errorf("mid must not be billed: %+v", rep.Groups)
	}
	if !strings.Contains(metricsText(env.metrics), `nautilm_rejected_total{agent="k8s",reason="on_behalf_of_not_allowed"} 1`) {
		t.Error("the ignored header must be counted")
	}
}

func TestUnpricedAndProviderReported(t *testing.T) {
	reported := 0.0042
	env := setup(t, &fake{}, &fake{reported: &reported})
	// cheap → provider b, mystery-model: no price in the catalog.
	resp := post(t, env.srv, "tok-k8s", fmt.Sprintf(hello, "cheap"))
	if resp.Header.Get("X-Nautilm-Price-Source") != "provider" || resp.Header.Get("X-Nautilm-Cost-USD") != "0.00420000" {
		t.Errorf("the cost the provider reported must win: %v", resp.Header)
	}

	env = setup(t, &fake{}, &fake{})
	resp = post(t, env.srv, "tok-k8s", fmt.Sprintf(hello, "cheap"))
	if resp.Header.Get("X-Nautilm-Price-Source") != "unpriced" || resp.Header.Get("X-Nautilm-Cost-USD") != "" {
		t.Errorf("an unpriced call must not claim a cost: %v", resp.Header)
	}
	rep := usageReport(t, env, "agent")
	if rep.Total.UnpricedCalls != 1 || rep.Total.CostUSD != 0 {
		t.Errorf("unpriced: %+v", rep.Total)
	}
	if !strings.Contains(metricsText(env.metrics), `nautilm_unpriced_calls_total{agent="k8s",provider="b",model="mystery-model"} 1`) {
		t.Error("unpriced metric missing")
	}
}

func TestUsageAPIProtection(t *testing.T) {
	env := setup(t, &fake{}, &fake{})
	if r := do(t, http.MethodGet, env.srv.URL+"/v1/usage", "", ""); r.StatusCode != 401 {
		t.Errorf("no token: %d", r.StatusCode)
	}
	if r := do(t, http.MethodGet, env.srv.URL+"/v1/usage", "tok-k8s", ""); r.StatusCode != 401 {
		t.Errorf("an agent token is not the dashboard token: %d", r.StatusCode)
	}
	if r := do(t, http.MethodGet, env.srv.URL+"/v1/usage?group_by=color", "dash-secret", ""); r.StatusCode != 400 {
		t.Errorf("invalid group_by: %d", r.StatusCode)
	}
	// Without dashboard.token the API is disabled, not open.
	noDash := setupCfg(t, strings.Replace(testConfig, "dashboard: {token: dash-secret}", "", 1),
		map[string]provider.Provider{"a": &fake{}, "b": &fake{}})
	if r := do(t, http.MethodGet, noDash.srv.URL+"/v1/usage", "anything", ""); r.StatusCode != 404 {
		t.Errorf("without a dashboard token: %d", r.StatusCode)
	}
}

func TestUsageCSVAndDashboard(t *testing.T) {
	env := setup(t, &fake{}, &fake{})
	post(t, env.srv, "tok-k8s", fmt.Sprintf(hello, "smart"))
	r := do(t, http.MethodGet, env.srv.URL+"/v1/usage.csv?group_by=agent,provider,model", "dash-secret", "")
	b, _ := io.ReadAll(r.Body)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if r.Header.Get("Content-Type") != "text/csv; charset=utf-8" || len(lines) != 2 ||
		!strings.HasPrefix(lines[0], "agent,provider,model,calls") || !strings.HasPrefix(lines[1], "k8s,a,big-model,1,0,10,3,0,0,0.000075,100.00") {
		t.Errorf("csv:\n%s", b)
	}
	d := do(t, http.MethodGet, env.srv.URL+"/dashboard", "", "")
	page, _ := io.ReadAll(d.Body)
	if d.StatusCode != 200 || !strings.Contains(string(page), "<title>Nautilus · Usage</title>") || d.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("dashboard: %d", d.StatusCode)
	}
}

// failingStore simulates Valkey down for the ledger.
type failingStore struct{}

func (failingStore) Add(context.Context, usage.Record) error { return errors.New("valkey down") }
func (failingStore) Rows(context.Context, time.Time, time.Time) ([]usage.Row, error) {
	return nil, errors.New("valkey down")
}

func TestLedgerDownDoesNotBreakCalls(t *testing.T) {
	env := setup(t, &fake{}, &fake{})
	env.gw.UseUsageStore(failingStore{})
	if r := post(t, env.srv, "tok-k8s", fmt.Sprintf(hello, "smart")); r.StatusCode != 200 {
		t.Fatalf("the call must succeed even with the ledger down: %d", r.StatusCode)
	}
	if !strings.Contains(metricsText(env.metrics), "nautilm_usage_lost_total 1") {
		t.Errorf("a lost record must be counted:\n%s", metricsText(env.metrics))
	}
	if r := do(t, http.MethodGet, env.srv.URL+"/v1/usage", "dash-secret", ""); r.StatusCode != 503 {
		t.Errorf("report with the ledger down: %d", r.StatusCode)
	}
}
