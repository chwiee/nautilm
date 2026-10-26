// agents simulates a few simple agents calling nautilm, each with its own
// token, until their quotas run out. It prints every call (status, tokens,
// cost, who was billed) and the real errors that show up, then a summary.
//
//	go run ./examples/agents                  # 4 agents, until the quotas run out
//	go run ./examples/agents -probes          # also fire the known error cases
//	go run ./examples/agents -rounds 3 -only k8s
//
// It talks plain OpenAI HTTP on purpose: no SDK, so what you see is what
// any agent would see.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type agent struct {
	Name     string
	Token    string
	Alias    string
	System   string
	Prompts  []string
	MaxToken int
}

// The agents of the example: different jobs, different token appetites.
func agents() []agent {
	tok := func(name string) string {
		if v := os.Getenv("NAUTILM_TOKEN_" + strings.ToUpper(name)); v != "" {
			return v
		}
		return "local-" + name
	}
	return []agent{
		{Name: "k8s", Token: tok("k8s"), Alias: "cheap", MaxToken: 300,
			System: "You are a Kubernetes troubleshooting assistant. Answer in 3 short bullet points.",
			Prompts: []string{
				"A pod is in CrashLoopBackOff with exit code 137. What are the likely causes?",
				"How do I see the logs of the previous container of a pod?",
				"What is the difference between a liveness and a readiness probe?",
				"A deployment is stuck at 0/3 ready replicas. What do I check first?",
			}},
		{Name: "mid", Token: tok("mid"), Alias: "cheap", MaxToken: 500,
			System: "You are a messaging middleware expert (RabbitMQ, IBM MQ). Be thorough.",
			Prompts: []string{
				"A RabbitMQ queue keeps growing and consumers are connected. Explain a full investigation plan.",
				"Explain IBM MQ error AMQ2058 and how to fix it, with examples.",
				"Compare quorum queues and classic mirrored queues in RabbitMQ in detail.",
			}},
		{Name: "marcia", Token: tok("marcia"), Alias: "cheap", MaxToken: 150,
			System: "You are a finance assistant. Answer in one sentence.",
			Prompts: []string{
				"What is a cost center?",
				"What is the difference between CAPEX and OPEX?",
				"What does chargeback mean in IT?",
			}},
		{Name: "claudia", Token: tok("claudia"), Alias: "cheap", MaxToken: 200,
			System: "You are a customer support assistant. Be polite and brief.",
			Prompts: []string{
				"A customer says the app is slow since yesterday. What do I ask them first?",
				"Write a short apology message for a 2-hour outage.",
			}},
	}
}

type result struct {
	Agent, Kind    string
	Status         int
	Code, Message  string
	In, Out        int
	CostUSD        string
	PriceSource    string
	BilledTo       string
	Provider       string
	Model          string
	QuotaDayLeft   string
	RetryAfter     string
	DurationMillis int64
}

var (
	url     = flag.String("url", "http://localhost:18080", "nautilm base URL")
	rounds  = flag.Int("rounds", 50, "max rounds per agent (stops earlier when the daily quota runs out)")
	only    = flag.String("only", "", "run only this agent")
	probes  = flag.Bool("probes", false, "also fire the known error cases (401, 403, 404, 400, on-behalf-of)")
	aliasF  = flag.String("alias", "", "use this alias for every agent (e.g. compare, to test the second model)")
	pause   = flag.Duration("pause", 500*time.Millisecond, "pause between calls of the same agent")
	client  = &http.Client{Timeout: 3 * time.Minute}
	printMu sync.Mutex
	allMu   sync.Mutex
	all     []result
)

func main() {
	flag.Parse()
	if *probes {
		runProbes()
	}
	var wg sync.WaitGroup
	for _, a := range agents() {
		if *only != "" && a.Name != *only {
			continue
		}
		if *aliasF != "" {
			a.Alias = *aliasF
		}
		wg.Add(1)
		go func(a agent) {
			defer wg.Done()
			runAgent(a)
		}(a)
	}
	wg.Wait()
	summary()
}

// runAgent asks its questions in a loop until the day quota stops it.
func runAgent(a agent) {
	minuteHits := 0
	for i := 0; i < *rounds*len(a.Prompts); i++ {
		body := map[string]any{"model": a.Alias, "max_tokens": a.MaxToken, "messages": []map[string]string{
			{"role": "system", "content": a.System},
			{"role": "user", "content": a.Prompts[i%len(a.Prompts)]},
		}}
		r := call(a.Name, "chat", a.Token, body, nil)
		record(r)
		if r.Status == http.StatusTooManyRequests && r.Code == "quota_day" {
			return // out of tokens for today: that is the point of the test
		}
		if r.Status == http.StatusTooManyRequests && r.Code == "quota_minute" {
			minuteHits++
			wait := 5 * time.Second
			if d, err := time.ParseDuration(r.RetryAfter + "s"); err == nil && d > 0 {
				wait = d
			}
			if minuteHits > 3 {
				return // enough minute-limit examples
			}
			time.Sleep(wait)
			continue
		}
		if r.Status >= 500 || r.Status == 0 {
			time.Sleep(3 * time.Second) // provider trouble: back off a little
		}
		time.Sleep(*pause)
	}
}

func call(name, kind, token string, body any, headers map[string]string) result {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, *url+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	t0 := time.Now()
	resp, err := client.Do(req)
	r := result{Agent: name, Kind: kind, DurationMillis: time.Since(t0).Milliseconds()}
	if err != nil {
		r.Message = err.Error()
		return r
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r.DurationMillis = time.Since(t0).Milliseconds()
	r.Status = resp.StatusCode
	h := resp.Header
	r.CostUSD, r.PriceSource, r.BilledTo = h.Get("X-Nautilm-Cost-USD"), h.Get("X-Nautilm-Price-Source"), h.Get("X-Nautilm-Billed-To")
	r.Provider, r.Model, r.QuotaDayLeft, r.RetryAfter = h.Get("X-Nautilm-Provider"), h.Get("X-Nautilm-Model"), h.Get("X-Nautilm-Quota-Day-Remaining"), h.Get("Retry-After")
	var out struct {
		Usage struct {
			In  int `json:"prompt_tokens"`
			Out int `json:"completion_tokens"`
		} `json:"usage"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(raw, &out)
	r.In, r.Out = out.Usage.In, out.Usage.Out
	r.Code, r.Message = out.Error.Code, out.Error.Message
	return r
}

func record(r result) {
	allMu.Lock()
	all = append(all, r)
	allMu.Unlock()
	printMu.Lock()
	defer printMu.Unlock()
	if r.Status == http.StatusOK {
		fmt.Printf("%-8s %-6s 200  in=%-5d out=%-4d cost=$%s (%s) billed_to=%s model=%s day_left=%s %dms\n",
			r.Agent, r.Kind, r.In, r.Out, r.CostUSD, r.PriceSource, r.BilledTo, r.Model, r.QuotaDayLeft, r.DurationMillis)
		return
	}
	extra := ""
	if r.RetryAfter != "" {
		extra = " retry_after=" + r.RetryAfter + "s"
	}
	fmt.Printf("%-8s %-6s %d  %s: %s%s\n", r.Agent, r.Kind, r.Status, r.Code, r.Message, extra)
}

// runProbes fires the error cases a real agent will meet sooner or later.
func runProbes() {
	fmt.Println("== error probes")
	hello := func(alias string) map[string]any {
		return map[string]any{"model": alias, "max_tokens": 20, "messages": []map[string]string{{"role": "user", "content": "Say OK."}}}
	}
	a := agents()
	cases := []struct {
		name, token string
		body        any
		headers     map[string]string
	}{
		{"no-token", "", hello("cheap"), nil},
		{"bad-token", "not-a-token", hello("cheap"), nil},
		{"unknown-alias", a[0].Token, hello("gpt-5"), nil},
		{"raw-model-id", a[0].Token, hello("google.gemma-3-4b-it"), nil},
		{"forbidden-alias", a[2].Token, hello("compare"), nil},
		{"empty-messages", a[0].Token, map[string]any{"model": "cheap", "messages": []any{}}, nil},
		{"bill-another-agent", a[2].Token, hello("cheap"), map[string]string{"X-Nautilm-On-Behalf-Of": "k8s"}},
	}
	for _, c := range cases {
		r := call(c.name, "probe", c.token, c.body, c.headers)
		record(r)
	}
	fmt.Println()
}

func summary() {
	type agg struct {
		calls, ok, in, out int
		cost               float64
		errs               map[string]int
	}
	by := map[string]*agg{}
	for _, r := range all {
		if r.Kind != "chat" {
			continue
		}
		g, ok := by[r.Agent]
		if !ok {
			g = &agg{errs: map[string]int{}}
			by[r.Agent] = g
		}
		g.calls++
		if r.Status == http.StatusOK {
			g.ok++
			g.in += r.In
			g.out += r.Out
			var c float64
			fmt.Sscanf(r.CostUSD, "%g", &c)
			g.cost += c
		} else {
			g.errs[fmt.Sprintf("%d %s", r.Status, r.Code)]++
		}
	}
	names := make([]string, 0, len(by))
	for n := range by {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Println("\n== summary (from the response headers; compare with /dashboard)")
	var tc float64
	var ti, to int
	for _, n := range names {
		g := by[n]
		var errs []string
		for k, v := range g.errs {
			errs = append(errs, fmt.Sprintf("%s ×%d", k, v))
		}
		sort.Strings(errs)
		fmt.Printf("%-8s calls=%-3d ok=%-3d in=%-6d out=%-6d cost=$%.6f  errors: %s\n", n, g.calls, g.ok, g.in, g.out, g.cost, strings.Join(errs, ", "))
		tc += g.cost
		ti += g.in
		to += g.out
	}
	fmt.Printf("%-8s tokens in=%d out=%d  cost=$%.6f\n", "TOTAL", ti, to, tc)
}
