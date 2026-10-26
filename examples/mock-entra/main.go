// mock-entra runs the lab-only OpenID Connect provider that behaves like
// Microsoft Entra ID (see internal/oidcmock). It lets the dashboard sign-in
// be tested end to end in the local cluster without a real tenant.
//
// NEVER deploy it outside a lab: its sign-in page lets anyone pick any user.
//
//	mock-entra -listen :8080 -public-url http://localhost:18096 -internal-url http://mock-entra.nautilm:8080
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/chwiee/nautilm/internal/oidcmock"
)

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	tenant := flag.String("tenant", "00000000-0000-0000-0000-00000000lab1", "tenant id (the tid claim)")
	clientID := flag.String("client-id", "nautilm-local", "client id of the nautilm app")
	publicURL := flag.String("public-url", "http://localhost:18096", "how the BROWSER reaches this provider")
	internalURL := flag.String("internal-url", "http://mock-entra:8080", "how NAUTILM reaches this provider (also the issuer)")
	adminGroup := flag.String("admin-group", "grp-nautilm-admins", "group object id of the admins")
	viewerGroup := flag.String("viewer-group", "grp-nautilm-viewers", "group object id of the viewers")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	secret := os.Getenv("MOCK_ENTRA_CLIENT_SECRET")
	if secret == "" {
		log.Error("MOCK_ENTRA_CLIENT_SECRET is required")
		os.Exit(1)
	}
	s, err := oidcmock.New(oidcmock.Options{Tenant: *tenant, ClientID: *clientID, ClientSecret: secret,
		PublicBase: *publicURL, InternalBase: *internalURL, Users: oidcmock.DefaultUsers(*adminGroup, *viewerGroup)})
	if err != nil {
		log.Error("start", "error", err)
		os.Exit(1)
	}
	log.Warn("mock Entra ID up: LAB ONLY, anyone can sign in as any user", "listen", *listen, "issuer", s.Issuer())
	if err := http.ListenAndServe(*listen, s.Handler()); err != nil {
		log.Error("stopped", "error", err)
		os.Exit(1)
	}
}
