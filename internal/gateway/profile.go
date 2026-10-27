package gateway

import (
	"slices"
	"sort"

	"github.com/chwiee/nautilm/internal/auth"
	"github.com/chwiee/nautilm/internal/live"
)

// An agent's permissions come from one of two places:
//   - the YAML ("agents:"), for ServiceAccounts and static tokens;
//   - the token's own profile, for tokens created from the dashboard.
// These helpers hide the difference from the rest of the gateway.

// allowed returns the aliases the caller may use (sorted).
func (g *Gateway) allowed(st *live.State, id auth.Identity) []string {
	if id.Profile == nil {
		return st.Cfg.Allowed(id.Agent)
	}
	var out []string
	for _, m := range id.Profile.Models {
		if m == "*" {
			out = out[:0]
			for alias := range st.Cfg.Models {
				out = append(out, alias)
			}
			break
		}
		if _, ok := st.Cfg.Models[m]; ok && !slices.Contains(out, m) {
			out = append(out, m) // an alias removed from the config later is ignored
		}
	}
	sort.Strings(out)
	return out
}

// quotaLimits returns the caller's quota (per minute, per day).
func (g *Gateway) quotaLimits(id auth.Identity) (int64, int64) {
	if id.Profile == nil {
		return g.cfg.Limits(id.Agent)
	}
	return g.cfg.LimitsWith(id.Profile.TokensPerMinute, id.Profile.TokensPerDay)
}

// billingOf returns team and cost center for the agent that pays. A token
// from the dashboard brings its own; otherwise the YAML.
func (g *Gateway) billingOf(id auth.Identity, payer string) (string, string) {
	if id.Profile != nil && payer == id.Agent {
		return id.Profile.Team, id.Profile.CostCenter
	}
	return g.cfg.Billing(payer)
}

// canActOnBehalf: only explicit YAML entries, never dashboard tokens (that
// permission is too powerful for a click).
func (g *Gateway) canActOnBehalf(id auth.Identity) bool {
	return id.Profile == nil && g.cfg.CanActOnBehalf(id.Agent)
}
