package config

import (
	"strings"
	"testing"
	"time"
)

func TestExampleIsValid(t *testing.T) {
	t.Setenv("NAUTILM_TOKEN_K8S", "x")
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml must stay valid: %v", err)
	}
	if missing := c.MissingPrices(time.Now()); len(missing) > 0 {
		t.Errorf("the example must price every model it uses: %v", missing)
	}
}

func TestValidatesReferences(t *testing.T) {
	_, err := Parse([]byte(`
providers: {p: {type: openai, base_url: http://x}}
models:
  a: {provider: missing, model: m, fallback: [b]}
agents:
  x: {models: [c]}
pricing:
  - {provider: nowhere, model: m, input: 1}
`))
	for _, want := range []string{`provider "missing"`, `fallback "b"`, `alias "c"`, `pricing[0]: provider "nowhere"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("expected an error containing %s, got: %v", want, err)
		}
	}
}

func TestExpandsVariables(t *testing.T) {
	t.Setenv("TOK", "abc")
	c, err := Parse([]byte(`
providers: {p: {type: openai, base_url: http://x}}
models: {a: {provider: p, model: m}}
auth: {modes: [tokens], tokens: {cli: "${TOK}"}}
`))
	if err != nil || c.Auth.Tokens["cli"] != "abc" {
		t.Fatalf("err=%v tokens=%v", err, c.Auth.Tokens)
	}
}

// Strict mode: a typo or an old pedagio key stops the startup.
func TestUnknownAndLegacyKeys(t *testing.T) {
	base := "providers: {p: {type: openai, base_url: http://x}}\nmodels: {a: {provider: p, model: m}}\n"
	_, err := Parse([]byte(base + "cotas: {tokens_por_minuto: 10}\n"))
	if err == nil || !strings.Contains(err.Error(), `use "quotas"`) {
		t.Errorf("legacy key: %v", err)
	}
	_, err = Parse([]byte(base + "agents: {k8s: {modles: [a]}}\n"))
	if err == nil || !strings.Contains(err.Error(), "modles") {
		t.Errorf("typo must fail: %v", err)
	}
}

func TestPricingAndStrict(t *testing.T) {
	cfg := `
providers: {bedrock: {type: bedrock}, ollama: {type: openai, base_url: http://x}}
models:
  smart: {provider: bedrock, model: sonnet}
  fast:  {provider: bedrock, model: nova}
  local: {provider: ollama, model: qwen}
pricing:
  - {provider: bedrock, model: sonnet, input: 3, output: 15}
  - {provider: ollama, model: "*", free: true}
`
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if m := c.MissingPrices(time.Now()); len(m) != 1 || m[0] != "bedrock/nova" {
		t.Errorf("missing: %v", m)
	}
	if _, err := Parse([]byte(cfg + "strict_pricing: true\n")); err == nil || !strings.Contains(err.Error(), "bedrock/nova") {
		t.Errorf("strict_pricing must fail naming the model: %v", err)
	}
}

func TestBillingAndOnBehalf(t *testing.T) {
	c, err := Parse([]byte(`
providers: {p: {type: openai, base_url: http://x}}
models: {a: {provider: p, model: m}}
agents:
  k8s:     {models: [a], team: ce, cost_center: "1234"}
  maestro: {models: [a], on_behalf_of: true}
  "*":     {models: [a], team: others, on_behalf_of: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	if team, cc := c.Billing("k8s"); team != "ce" || cc != "1234" {
		t.Errorf("billing: %s %s", team, cc)
	}
	if team, _ := c.Billing("someone"); team != "others" {
		t.Errorf("unlisted agents use the * entry: %s", team)
	}
	if !c.CanActOnBehalf("maestro") || c.CanActOnBehalf("k8s") || c.CanActOnBehalf("someone") {
		t.Error("on_behalf_of only from an explicit entry, never from *")
	}
}
