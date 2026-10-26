package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"fmt"
	"net/http"

	"github.com/chwiee/nautilm/internal/auth"
	"github.com/chwiee/nautilm/internal/usage"
)

//go:embed web/dashboard.html
var dashboardHTML []byte

// dashboard serves the page. The page itself holds no data: it asks for the
// dashboard token and reads /v1/usage with it.
func (g *Gateway) dashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The page only talks to this same origin.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
	w.Write(dashboardHTML)
}

// usageAllowed checks the dashboard token. Cost data is internal: without
// dashboard.token configured, the usage API is disabled (404), not open.
func (g *Gateway) usageAllowed(w http.ResponseWriter, r *http.Request) bool {
	if g.usage == nil {
		writeError(w, http.StatusNotFound, "usage_disabled", "the usage ledger is disabled (usage.disabled: true)")
		return false
	}
	want := g.cfg.Dashboard.Token
	if want == "" {
		writeError(w, http.StatusNotFound, "usage_api_disabled", "set dashboard.token in the nautilm config to enable /v1/usage")
		return false
	}
	got := auth.BearerToken(r)
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	if got == "" || subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
		g.metrics.Rejected("", "dashboard_unauthenticated")
		writeError(w, http.StatusUnauthorized, "unauthenticated", "send Authorization: Bearer <dashboard token>")
		return false
	}
	return true
}

func (g *Gateway) usageReport(w http.ResponseWriter, r *http.Request) (usage.Report, bool) {
	if !g.usageAllowed(w, r) {
		return usage.Report{}, false
	}
	qs := r.URL.Query()
	q, err := usage.ParseQuery(qs.Get("from"), qs.Get("to"), qs.Get("group_by"), g.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return usage.Report{}, false
	}
	rep, err := usage.Build(r.Context(), g.usage, q)
	if err != nil {
		g.log.Error("usage report failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "usage_unavailable", "could not read the usage ledger right now")
		return usage.Report{}, false
	}
	return rep, true
}

// usageJSON serves GET /v1/usage?from=YYYY-MM-DD&to=YYYY-MM-DD&group_by=agent,provider
func (g *Gateway) usageJSON(w http.ResponseWriter, r *http.Request) {
	rep, ok := g.usageReport(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, rep)
}

// usageCSV serves the same report as CSV (finance).
func (g *Gateway) usageCSV(w http.ResponseWriter, r *http.Request) {
	rep, ok := g.usageReport(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="nautilm-usage-%s-to-%s.csv"`, rep.From, rep.To))
	if err := usage.WriteCSV(w, rep); err != nil {
		g.log.Warn("usage csv write failed", "error", err)
	}
}
