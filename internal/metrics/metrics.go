// Package metrics exposes counters in the Prometheus text format, with no
// external dependency. It is for OPERATIONS (Grafana, alerts). Billing
// lives in the usage ledger (internal/usage), which keeps months of totals.
//
//	nautilm_requests_total{agent,alias,provider,model,status}
//	nautilm_tokens_total{agent,alias,provider,model,kind="input|output|cache_read|cache_write"}
//	nautilm_cost_usd_total{agent,provider,model,source}
//	nautilm_request_duration_seconds_sum / _count{agent,alias,provider,model}
//	nautilm_time_to_first_token_seconds_sum / _count{agent,alias,model}
//	nautilm_fallbacks_total{agent,from,to}
//	nautilm_rejected_total{agent,reason}
//	nautilm_masked_total{agent,rule}
//	nautilm_usage_lost_total (usage records that could not be stored)
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Registry struct {
	mu       sync.Mutex
	counters map[string]float64 // key = name{labels}
}

func New() *Registry { return &Registry{counters: map[string]float64{}} }

// Call records a call to a model. agent is who pays (see usage.Record).
func (r *Registry) Call(agent, alias, provider, model string, status int, d time.Duration, in, out, cacheRead, cacheWrite int) {
	l := labels("agent", agent, "alias", alias, "provider", provider, "model", model)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["nautilm_requests_total"+labels("agent", agent, "alias", alias, "provider", provider, "model", model, "status", fmt.Sprint(status))]++
	r.counters["nautilm_request_duration_seconds_sum"+l] += d.Seconds()
	r.counters["nautilm_request_duration_seconds_count"+l]++
	for kind, n := range map[string]int{"input": in, "output": out, "cache_read": cacheRead, "cache_write": cacheWrite} {
		if n > 0 {
			r.counters["nautilm_tokens_total"+labels("agent", agent, "alias", alias, "provider", provider, "model", model, "kind", kind)] += float64(n)
		}
	}
}

// Cost adds the estimated cost in USD. source = table | provider (free and
// unpriced calls add nothing here; unpriced ones are counted by Unpriced).
func (r *Registry) Cost(agent, provider, model, source string, usd float64) {
	if usd <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["nautilm_cost_usd_total"+labels("agent", agent, "provider", provider, "model", model, "source", source)] += usd
}

// Unpriced counts calls whose model has no price (cost unknown).
func (r *Registry) Unpriced(agent, provider, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["nautilm_unpriced_calls_total"+labels("agent", agent, "provider", provider, "model", model)]++
}

// FirstToken records the time until the 1st chunk reached the agent
// (streaming only). It is the latency the user feels in a chat.
func (r *Registry) FirstToken(agent, alias, model string, d time.Duration) {
	l := labels("agent", agent, "alias", alias, "model", model)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["nautilm_time_to_first_token_seconds_sum"+l] += d.Seconds()
	r.counters["nautilm_time_to_first_token_seconds_count"+l]++
}

// Fallback records that alias "from" failed and "to" was tried.
func (r *Registry) Fallback(agent, from, to string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["nautilm_fallbacks_total"+labels("agent", agent, "from", from, "to", to)]++
}

// Rejected records requests stopped before reaching a model (401/403/400/429).
func (r *Registry) Rejected(agent, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["nautilm_rejected_total"+labels("agent", agent, "reason", reason)]++
}

// Masked counts how much sensitive data each rule removed, per agent.
// Useful to find the agent that keeps sending secrets in prompts.
func (r *Registry) Masked(agent, rule string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["nautilm_masked_total"+labels("agent", agent, "rule", rule)] += float64(n)
}

// UsageLost counts usage records that could not be stored (Valkey down).
// They are still in the log ("usage record lost"), so they can be recovered.
func (r *Registry) UsageLost() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["nautilm_usage_lost_total{}"]++
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	r.Write(w)
}

func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	keys := make([]string, 0, len(r.counters))
	for k := range r.counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]float64, len(keys))
	for i, k := range keys {
		vals[i] = r.counters[k]
	}
	r.mu.Unlock()

	lastName := ""
	for i, k := range keys {
		name := k[:strings.IndexByte(k, '{')]
		if name != lastName {
			fmt.Fprintf(w, "# TYPE %s counter\n", name)
			lastName = name
		}
		if strings.HasSuffix(k, "{}") {
			k = name
		}
		fmt.Fprintf(w, "%s %g\n", k, vals[i])
	}
}

func labels(kv ...string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", kv[i], kv[i+1])
	}
	b.WriteByte('}')
	return b.String()
}
