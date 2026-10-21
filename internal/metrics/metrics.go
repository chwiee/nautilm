// Package metrics expõe contadores no formato texto do Prometheus, sem
// dependência externa. É daqui que sai "quanto cada agente/time gasta".
//
//	pedagio_requests_total{agent,alias,model,status}
//	pedagio_tokens_total{agent,alias,model,kind="input|output|cached"}
//	pedagio_request_duration_seconds_sum / _count{agent,alias,model}
//	pedagio_time_to_first_token_seconds_sum / _count{agent,alias,model}
//	pedagio_fallbacks_total{agent,from,to}
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
	counters map[string]float64 // chave = nome{labels}
}

func New() *Registry { return &Registry{counters: map[string]float64{}} }

// Call registra uma chamada a um modelo.
func (r *Registry) Call(agent, alias, model string, status int, d time.Duration, in, out, cached int) {
	l := labels("agent", agent, "alias", alias, "model", model)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["pedagio_requests_total"+labels("agent", agent, "alias", alias, "model", model, "status", fmt.Sprint(status))]++
	r.counters["pedagio_request_duration_seconds_sum"+l] += d.Seconds()
	r.counters["pedagio_request_duration_seconds_count"+l]++
	if in > 0 {
		r.counters["pedagio_tokens_total"+labels("agent", agent, "alias", alias, "model", model, "kind", "input")] += float64(in)
	}
	if out > 0 {
		r.counters["pedagio_tokens_total"+labels("agent", agent, "alias", alias, "model", model, "kind", "output")] += float64(out)
	}
	if cached > 0 {
		r.counters["pedagio_tokens_total"+labels("agent", agent, "alias", alias, "model", model, "kind", "cached")] += float64(cached)
	}
}

// FirstToken registra o tempo até o 1º chunk chegar ao agente (só streaming).
// É a latência que o usuário sente no chat.
func (r *Registry) FirstToken(agent, alias, model string, d time.Duration) {
	l := labels("agent", agent, "alias", alias, "model", model)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["pedagio_time_to_first_token_seconds_sum"+l] += d.Seconds()
	r.counters["pedagio_time_to_first_token_seconds_count"+l]++
}

// Fallback registra que o apelido "from" falhou e "to" foi tentado.
func (r *Registry) Fallback(agent, from, to string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["pedagio_fallbacks_total"+labels("agent", agent, "from", from, "to", to)]++
}

// Rejected registra pedidos barrados antes de chegar a um modelo (401/403/400).
func (r *Registry) Rejected(agent, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["pedagio_rejected_total"+labels("agent", agent, "reason", reason)]++
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
