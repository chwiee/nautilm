package gateway

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/chwiee/pedagio/internal/auth"
	"github.com/chwiee/pedagio/internal/quota"
)

// UseQuotaCounter troca o contador das cotas (ex: Valkey no lugar da memória).
func (g *Gateway) UseQuotaCounter(c quota.Counter) {
	if g.quota != nil {
		g.quota.Counter = c
	}
}

func (g *Gateway) limits(id auth.Identity) quota.Limits {
	pm, pd := g.cfg.Limits(id.Agent)
	return quota.Limits{PerMinute: pm, PerDay: pd}
}

// checkQuota devolve false (e já respondeu 429) se o agente estourou a cota.
// Sem fallback de propósito: outro modelo gastaria do MESMO orçamento.
func (g *Gateway) checkQuota(w http.ResponseWriter, r *http.Request, id auth.Identity, l quota.Limits) bool {
	if g.quota == nil || (l.PerMinute <= 0 && l.PerDay <= 0) {
		return true
	}
	d, err := g.quota.Check(r.Context(), id.Agent, l)
	if err != nil {
		// fail-open: cota protege custo, não pode derrubar os agentes.
		g.log.Warn("cota indisponível, deixando passar", "agent", id.Agent, "error", err)
		return true
	}
	if d.DayLeft >= 0 {
		w.Header().Set("X-Pedagio-Quota-Day-Remaining", strconv.FormatInt(d.DayLeft, 10))
	}
	if d.Allowed {
		return true
	}
	secs := int(math.Ceil(d.RetryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	limit := l.PerMinute
	if d.Window == "dia" {
		limit = l.PerDay
	}
	g.reject(w, id, http.StatusTooManyRequests, "quota_"+map[string]string{"minuto": "minute", "dia": "day"}[d.Window],
		fmt.Sprintf("agente %q passou da cota de %d tokens por %s; tente de novo em %ds", id.Agent, limit, d.Window, secs))
	return false
}

// spendQuota soma o gasto. Usa um contexto que não morre com o pedido:
// se o agente desconectar no fim do stream, os tokens foram gastos mesmo assim.
func (g *Gateway) spendQuota(ctx context.Context, id auth.Identity, l quota.Limits, tokens int) {
	if g.quota == nil || tokens <= 0 {
		return
	}
	if err := g.quota.Spend(context.WithoutCancel(ctx), id.Agent, l, int64(tokens)); err != nil {
		g.log.Warn("cota: não consegui somar o gasto", "agent", id.Agent, "tokens", tokens, "error", err)
	}
}
