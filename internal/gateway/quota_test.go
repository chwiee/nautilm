package gateway

import (
	"fmt"
	"strings"
	"testing"

	"github.com/chwiee/nautilm/internal/config"
	"github.com/chwiee/nautilm/internal/provider"
)

// The fake answers 10 input + 3 output = 13 tokens per call.
const quotaConfig = `
providers:
  a: {type: openai, base_url: http://x}
  b: {type: openai, base_url: http://y}
models:
  smart: {provider: a, model: big-model, fallback: [fast]}
  fast:  {provider: b, model: small-model}
auth:
  modes: [tokens]
  tokens: {k8s: tok-k8s, cli: tok-cli}
quotas: {tokens_per_minute: 20}
agents:
  k8s: {models: [smart, fast], tokens_per_minute: -1, tokens_per_day: 30}
  "*": {models: [fast]}
`

func TestQuotaPerMinute429(t *testing.T) {
	b := &fake{}
	env := setupCfg(t, quotaConfig, map[string]provider.Provider{"a": &fake{}, "b": b})
	body := fmt.Sprintf(hello, "fast")

	if r := post(t, env.srv, "tok-cli", body); r.StatusCode != 200 {
		t.Fatalf("1st: %d", r.StatusCode)
	}
	if r := post(t, env.srv, "tok-cli", body); r.StatusCode != 200 {
		t.Fatalf("2nd (13 < 20, still passes): %d", r.StatusCode)
	}
	r := post(t, env.srv, "tok-cli", body) // 26 >= 20
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("3rd must be 429 with Retry-After: %d %v", r.StatusCode, r.Header)
	}
	if len(b.calls) != 2 {
		t.Errorf("a 429 must not reach the provider: %d calls", len(b.calls))
	}
	if !strings.Contains(metricsText(env.metrics), `nautilm_rejected_total{agent="cli",reason="quota_minute"} 1`) {
		t.Errorf("metric:\n%s", metricsText(env.metrics))
	}
}

func TestAgentQuotaOverridesDefault(t *testing.T) {
	env := setupCfg(t, quotaConfig, map[string]provider.Provider{"a": &fake{}, "b": &fake{}})
	body := fmt.Sprintf(hello, "smart")
	// k8s: no per-minute limit (-1), but 30 per day.
	for i, want := range []int{200, 200, 200, 429} {
		r := post(t, env.srv, "tok-k8s", body)
		if r.StatusCode != want {
			t.Fatalf("call %d: %d, want %d", i+1, r.StatusCode, want)
		}
		if want == 200 && r.Header.Get("X-Nautilm-Quota-Day-Remaining") == "" {
			t.Error("missing the remaining-quota header")
		}
	}
}

func TestLimitsConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(quotaConfig))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]int64{
		"k8s":     {0, 30},                          // -1 = no limit
		"someone": {20, config.DefaultTokensPerDay}, // global + default
	}
	for id, want := range cases {
		if pm, pd := cfg.Limits(id); pm != want[0] || pd != want[1] {
			t.Errorf("%s: %d/%d, want %v", id, pm, pd, want)
		}
	}
	cfg.Quotas.Disabled = true
	if pm, pd := cfg.Limits("someone"); pm != 0 || pd != 0 {
		t.Error("disabled = no limit")
	}
}
