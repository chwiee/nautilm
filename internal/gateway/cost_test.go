package gateway

import (
	"bytes"
	"strings"
	"testing"
)

func TestCustoPorApelido(t *testing.T) {
	cfg := strings.Replace(testConfig, "fast:  {provider: b, model: modelo-pequeno}",
		"fast:  {provider: b, model: modelo-pequeno, preco: {entrada: 3, saida: 15}}", 1)
	srv, m := setupCfg(t, cfg, &fake{}, &fake{})
	post(t, srv, "tok-cli", `{"model":"fast","messages":[{"role":"user","content":"oi"}]}`)
	post(t, srv, "tok-cli", `{"model":"fast","messages":[{"role":"user","content":"oi"}]}`)
	var buf bytes.Buffer
	m.Write(&buf)
	// 2 chamadas × (10 entrada × 3 + 3 saída × 15) / 1 milhão = 0,00015 USD
	if !strings.Contains(buf.String(), `pedagio_cost_usd_total{agent="cli",alias="fast",model="modelo-pequeno"} 0.00015`) {
		t.Errorf("custo:\n%s", buf.String())
	}
	// smart não tem preço: não aparece (custo 0, ex: Ollama).
	if strings.Contains(buf.String(), `cost_usd_total{agent="kubets"`) {
		t.Error("sem preço = sem série de custo")
	}
}
