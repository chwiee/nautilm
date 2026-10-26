// Package oidcmock is a tiny OpenID Connect provider that behaves like
// Microsoft Entra ID for the parts nautilm uses: discovery, authorize (with
// PKCE), token (RS256 ID token with oid/tid/groups) and JWKS.
//
// It exists so the sign-in can be tested end to end (unit tests and the
// local cluster) without a real tenant. NEVER expose it outside a lab: the
// sign-in page lets anyone pick any user.
//
// Two base URLs, because in a cluster the browser and nautilm see the
// provider at different addresses:
//
//	PublicBase:   what the browser opens (authorize)
//	InternalBase: what nautilm calls (discovery, token, keys) and the issuer
package oidcmock

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type User struct {
	Key     string // what the sign-in page shows
	Name    string
	Email   string
	OID     string
	Groups  []string
	Overage bool // simulate more than 200 groups (no "groups" claim)
}

type Options struct {
	Tenant       string
	ClientID     string
	ClientSecret string
	PublicBase   string
	InternalBase string
	Users        []User
}

type Server struct {
	opt   Options
	key   *rsa.PrivateKey
	kid   string
	mu    sync.Mutex
	codes map[string]grant
}

type grant struct {
	user                       User
	nonce, challenge, redirect string
	expires                    time.Time
}

func New(opt Options) (*Server, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	opt.PublicBase = strings.TrimRight(opt.PublicBase, "/")
	opt.InternalBase = strings.TrimRight(opt.InternalBase, "/")
	return &Server{opt: opt, key: k, kid: "mock-key-1", codes: map[string]grant{}}, nil
}

// Issuer is what nautilm must use as issuer_url.
func (s *Server) Issuer() string { return s.opt.InternalBase + "/" + s.opt.Tenant + "/v2.0" }

// DefaultUsers covers the cases nautilm must handle.
func DefaultUsers(adminGroup, viewerGroup string) []User {
	return []User{
		{Key: "ana", Name: "Ana Admin", Email: "ana@corp.example", OID: "oid-ana", Groups: []string{adminGroup, "other"}},
		{Key: "bruno", Name: "Bruno Viewer", Email: "bruno@corp.example", OID: "oid-bruno", Groups: []string{viewerGroup}},
		{Key: "carla", Name: "Carla NoGroup", Email: "carla@corp.example", OID: "oid-carla", Groups: []string{"unrelated"}},
		{Key: "dani", Name: "Dani ManyGroups", Email: "dani@corp.example", OID: "oid-dani", Overage: true},
	}
}

func (s *Server) Handler() http.Handler {
	t := "/" + s.opt.Tenant
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+t+"/v2.0/.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("GET "+t+"/oauth2/v2.0/authorize", s.authorize)
	mux.HandleFunc("POST "+t+"/oauth2/v2.0/token", s.token)
	mux.HandleFunc("GET "+t+"/discovery/v2.0/keys", s.keys)
	return mux
}

func (s *Server) discovery(w http.ResponseWriter, _ *http.Request) {
	t := "/" + s.opt.Tenant
	writeJSON(w, 200, map[string]any{
		"issuer":                                s.Issuer(),
		"authorization_endpoint":                s.opt.PublicBase + t + "/oauth2/v2.0/authorize",
		"token_endpoint":                        s.opt.InternalBase + t + "/oauth2/v2.0/token",
		"jwks_uri":                              s.opt.InternalBase + t + "/discovery/v2.0/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"pairwise"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

var signInPage = template.Must(template.New("s").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>Mock Entra ID</title>
<style>body{font-family:system-ui,sans-serif;background:#f3f2f1;display:grid;place-items:center;min-height:100vh;margin:0;padding:16px}
.b{background:#fff;padding:24px;border-radius:4px;box-shadow:0 2px 6px rgba(0,0,0,.2);width:min(380px,100%)}
a{display:block;margin:8px 0;padding:10px;border:1px solid #8a8886;color:#323130;text-decoration:none}
a:hover{background:#f3f2f1}small{color:#605e5c}</style></head><body><div class="b">
<h2>Mock Entra ID</h2><p><small>LAB ONLY: pick who signs in.</small></p>
{{range .Users}}<a href="{{.Link}}" id="user-{{.Key}}">{{.Name}} <small>{{.Email}} · {{.Desc}}</small></a>{{end}}
</div></body></html>`))

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != s.opt.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("redirect_uri") == "" {
		http.Error(w, "invalid authorize request (client_id, response_type=code, PKCE S256 and redirect_uri are required)", 400)
		return
	}
	if key := q.Get("user"); key != "" {
		for _, u := range s.opt.Users {
			if u.Key == key {
				code := randomString()
				s.mu.Lock()
				s.codes[code] = grant{user: u, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
					redirect: q.Get("redirect_uri"), expires: time.Now().Add(2 * time.Minute)}
				s.mu.Unlock()
				dest, _ := url.Parse(q.Get("redirect_uri"))
				dq := dest.Query()
				dq.Set("code", code)
				dq.Set("state", q.Get("state"))
				dest.RawQuery = dq.Encode()
				http.Redirect(w, r, dest.String(), http.StatusFound)
				return
			}
		}
	}
	type row struct{ Key, Name, Email, Desc, Link string }
	var rows []row
	for _, u := range s.opt.Users {
		q2 := r.URL.Query()
		q2.Set("user", u.Key)
		desc := "groups: " + strings.Join(u.Groups, ", ")
		if u.Overage {
			desc = "more than 200 groups"
		}
		rows = append(rows, row{u.Key, u.Name, u.Email, desc, r.URL.Path + "?" + q2.Encode()})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	signInPage.Execute(w, map[string]any{"Users": rows})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != s.opt.ClientID || secret != s.opt.ClientSecret {
		writeJSON(w, 401, map[string]string{"error": "invalid_client"})
		return
	}
	s.mu.Lock()
	g, ok := s.codes[r.PostForm.Get("code")]
	delete(s.codes, r.PostForm.Get("code")) // a code is single-use
	s.mu.Unlock()
	if !ok || time.Now().After(g.expires) || g.redirect != r.PostForm.Get("redirect_uri") {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "PKCE verification failed"})
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": s.Issuer(), "sub": g.user.OID, "aud": s.opt.ClientID, "iat": now.Unix(), "nbf": now.Unix(),
		"exp": now.Add(time.Hour).Unix(), "nonce": g.nonce, "oid": g.user.OID, "tid": s.opt.Tenant,
		"name": g.user.Name, "preferred_username": g.user.Email, "email": g.user.Email,
	}
	if g.user.Overage {
		// Exactly what Entra sends with more than 200 groups: no "groups",
		// a pointer to Microsoft Graph instead.
		claims["_claim_names"] = map[string]string{"groups": "src1"}
		claims["_claim_sources"] = map[string]any{"src1": map[string]string{
			"endpoint": "https://graph.windows.net/" + s.opt.Tenant + "/users/" + g.user.OID + "/getMemberObjects"}}
	} else {
		claims["groups"] = g.user.Groups
	}
	idToken, err := s.sign(claims)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "server_error"})
		return
	}
	writeJSON(w, 200, map[string]any{"access_token": "mock-access-token", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
}

func (s *Server) keys(w http.ResponseWriter, _ *http.Request) {
	pub := s.key.PublicKey
	writeJSON(w, 200, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": s.kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (s *Server) sign(claims map[string]any) (string, error) {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.kid})
	c, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func randomString() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// String helps debugging.
func (u User) String() string { return fmt.Sprintf("%s <%s>", u.Name, u.Email) }
