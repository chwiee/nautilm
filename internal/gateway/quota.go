package gateway

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/chwiee/nautilm/internal/auth"
	"github.com/chwiee/nautilm/internal/quota"
)

// The quota is always the CALLER's (the authenticated identity), even with
// on-behalf-of: it protects against the caller's own loop.
func (g *Gateway) limits(id auth.Identity) quota.Limits {
	pm, pd := g.cfg.Limits(id.Agent)
	return quota.Limits{PerMinute: pm, PerDay: pd}
}

// checkQuota returns false (and has already answered 429) when the agent ran
// out of quota. No fallback on purpose: another model would spend from the
// SAME budget.
func (g *Gateway) checkQuota(w http.ResponseWriter, r *http.Request, id auth.Identity, l quota.Limits) bool {
	if g.quota == nil || (l.PerMinute <= 0 && l.PerDay <= 0) {
		return true
	}
	d, err := g.quota.Check(r.Context(), id.Agent, l)
	if err != nil {
		// fail-open: quotas protect cost, they must not take the agents down.
		g.log.Warn("quota unavailable, letting the call through", "agent", id.Agent, "error", err)
		return true
	}
	if d.DayLeft >= 0 {
		w.Header().Set("X-Nautilm-Quota-Day-Remaining", strconv.FormatInt(d.DayLeft, 10))
	}
	if d.Allowed {
		return true
	}
	secs := int(math.Ceil(d.RetryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	limit := l.PerMinute
	if d.Window == "day" {
		limit = l.PerDay
	}
	g.reject(w, id, http.StatusTooManyRequests, "quota_"+d.Window,
		fmt.Sprintf("agent %q exceeded its quota of %d tokens per %s; retry in %ds", id.Agent, limit, d.Window, secs))
	return false
}

// spendQuota adds the spend. It uses a context that does not die with the
// request: if the agent disconnects at the end of a stream, the tokens were
// spent anyway.
func (g *Gateway) spendQuota(ctx context.Context, id auth.Identity, l quota.Limits, tokens int) {
	if g.quota == nil || tokens <= 0 {
		return
	}
	if err := g.quota.Spend(context.WithoutCancel(ctx), id.Agent, l, int64(tokens)); err != nil {
		g.log.Warn("quota: could not add the spend", "agent", id.Agent, "tokens", tokens, "error", err)
	}
}
