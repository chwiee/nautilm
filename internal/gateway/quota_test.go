package gateway

import (
	"bytes"
	"strings"
	"testing"

	"github.com/chwiee/pedagio/internal/config"
)

// O fake responde com 10 tokens de entrada + 3 de saída = 13 por chamada.
const quotaConfig = `
providers:
  a: {type: openai, base_url: http://x}
  b: {type: openai, base_url: http://y}
models:
  smart: {provider: a, model: modelo-grande, fallback: [fast]}
  fast:  {provider: b, model: modelo-pequeno}
auth:
  modes: [tokens]
  tokens: {kubets: tok-kubets, cli: tok-cli}
cotas: {tokens_por_minuto: 20}
agents:
  kubets: {models: [smart, fast], tokens_por_minuto: -1, tokens_por_dia: 30}
  "*":    {models: [fast]}
`

func TestCotaPorMinuto429(t *testing.T) {
	a, b := &fake{}, &fake{}
	srv, m := setupCfg(t, quotaConfig, a, b)
	body := `{"model":"fast","messages":[{"role":"user","content":"oi"}]}`

	if r := post(t, srv, "tok-cli", body); r.StatusCode != 200 {
		t.Fatalf("1ª: %d", r.StatusCode)
	}
	if r := post(t, srv, "tok-cli", body); r.StatusCode != 200 {
		t.Fatalf("2ª (13 < 20, ainda passa): %d", r.StatusCode)
	}
	r := post(t, srv, "tok-cli", body) // 26 >= 20
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("3ª deveria ser 429 com Retry-After: %d %v", r.StatusCode, r.Header)
	}
	if len(b.calls) != 2 {
		t.Errorf("o 429 não pode chegar ao provider: %d chamadas", len(b.calls))
	}
	var buf bytes.Buffer
	m.Write(&buf)
	if !strings.Contains(buf.String(), `pedagio_rejected_total{agent="cli",reason="quota_minute"} 1`) {
		t.Errorf("métrica:\n%s", buf.String())
	}
}

func TestCotaDoAgenteSobrescreve(t *testing.T) {
	a, b := &fake{}, &fake{}
	srv, _ := setupCfg(t, quotaConfig, a, b)
	body := `{"model":"smart","messages":[{"role":"user","content":"oi"}]}`
	// kubets: sem limite por minuto (-1), mas 30 por dia.
	for i, want := range []int{200, 200, 200, 429} {
		r := post(t, srv, "tok-kubets", body)
		if r.StatusCode != want {
			t.Fatalf("chamada %d: %d, queria %d", i+1, r.StatusCode, want)
		}
		if want == 200 && r.Header.Get("X-Pedagio-Quota-Day-Remaining") == "" {
			t.Error("faltou o header de cota restante")
		}
	}
}

func TestLimitsConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(quotaConfig))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]int64{
		"kubets":   {0, 30},                          // -1 = sem limite
		"qualquer": {20, config.DefaultTokensPorDia}, // global + padrão
	}
	for id, want := range cases {
		if pm, pd := cfg.Limits(id); pm != want[0] || pd != want[1] {
			t.Errorf("%s: %d/%d, queria %v", id, pm, pd, want)
		}
	}
	cfg.Cotas.Desligado = true
	if pm, pd := cfg.Limits("qualquer"); pm != 0 || pd != 0 {
		t.Error("desligado = sem limite")
	}
}
