package auth

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Default token/CA paths mounted in every pod.
const (
	saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// Kubernetes validates ServiceAccount tokens via TokenReview using only the
// stdlib (no client-go: fewer dependencies, smaller image).
//
// nautilm needs the "system:auth-delegator" ClusterRole (allows creating
// TokenReviews): see deployments/k8s/nautilm.yaml.
type Kubernetes struct {
	APIServer string // default: https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT
	Audience  string // the agent token must carry this audience
	TTL       time.Duration

	http      *http.Client
	readToken func() (string, error) // nautilm's OWN token (renewed on disk)

	mu    sync.Mutex
	cache map[[32]byte]cached
}

type cached struct {
	id      Identity
	expires time.Time
}

// NewKubernetesInCluster configures itself from the pod environment.
func NewKubernetesInCluster(audience string) (*Kubernetes, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, fmt.Errorf("auth kubernetes: KUBERNETES_SERVICE_HOST is empty (running outside the cluster? use auth.modes: [tokens])")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("auth kubernetes: reading CA: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	k := &Kubernetes{
		APIServer: "https://" + host + ":" + port,
		Audience:  audience,
		http: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}},
		readToken: func() (string, error) {
			b, err := os.ReadFile(saDir + "/token")
			return strings.TrimSpace(string(b)), err
		},
	}
	return k, nil
}

func (k *Kubernetes) Authenticate(r *http.Request) (Identity, error) {
	tok := BearerToken(r)
	if !looksLikeJWT(tok) {
		// A ServiceAccount token is always a JWT. Anything else (a static
		// token, naut_...) is not ours: let the next authenticator try,
		// whatever the order in auth.modes, and skip a useless TokenReview.
		return Identity{}, ErrNoCredentials
	}
	key := sha256.Sum256([]byte(tok))
	if id, ok := k.fromCache(key); ok {
		return id, nil
	}
	id, err := k.review(tok)
	if err != nil {
		return Identity{}, err
	}
	k.toCache(key, id)
	return id, nil
}

func (k *Kubernetes) review(tok string) (Identity, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenReview",
		"spec":       map[string]any{"token": tok, "audiences": []string{k.Audience}},
	})
	req, err := http.NewRequest(http.MethodPost, k.APIServer+"/apis/authentication.k8s.io/v1/tokenreviews", bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	own, err := k.readToken()
	if err != nil {
		return Identity{}, fmt.Errorf("auth kubernetes: own token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+own)
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.http.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("auth kubernetes: TokenReview: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("auth kubernetes: TokenReview HTTP %d (missing ClusterRole system:auth-delegator?)", resp.StatusCode)
	}
	var out struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Identity{}, fmt.Errorf("auth kubernetes: response: %w", err)
	}
	if !out.Status.Authenticated {
		return Identity{}, ErrInvalid
	}
	// username = system:serviceaccount:<namespace>:<name>
	const prefix = "system:serviceaccount:"
	u := out.Status.User.Username
	if !strings.HasPrefix(u, prefix) {
		return Identity{}, ErrInvalid // ServiceAccounts only, not human users
	}
	return Identity{Agent: strings.Replace(strings.TrimPrefix(u, prefix), ":", "/", 1), Method: "kubernetes"}, nil
}

// looksLikeJWT: three non-empty base64url parts separated by dots.
func looksLikeJWT(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.Trim(p, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_=") != "" {
			return false
		}
	}
	return true
}

func (k *Kubernetes) fromCache(key [32]byte) (Identity, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	c, ok := k.cache[key]
	if !ok || time.Now().After(c.expires) {
		return Identity{}, false
	}
	return c.id, true
}

// toCache avoids one TokenReview per LLM call. Short TTL (1 min) so a
// removed ServiceAccount loses access quickly.
func (k *Kubernetes) toCache(key [32]byte, id Identity) {
	ttl := k.TTL
	if ttl == 0 {
		ttl = time.Minute
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cache == nil || len(k.cache) > 10_000 {
		k.cache = map[[32]byte]cached{} // simple cleanup: drop everything
	}
	k.cache[key] = cached{id: id, expires: time.Now().Add(ttl)}
}
