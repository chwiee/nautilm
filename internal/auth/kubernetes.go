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

// Caminhos padrão do token/CA montados em todo pod.
const (
	saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// Kubernetes valida tokens de ServiceAccount via TokenReview, só com stdlib
// (sem client-go: menos dependência, imagem menor).
//
// O pedagio precisa da ClusterRole "system:auth-delegator" (permite criar
// TokenReviews) — está em deployments/k8s/pedagio.yaml.
type Kubernetes struct {
	APIServer string // padrão: https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT
	Audience  string // o token do agente precisa ter esta audience
	TTL       time.Duration

	http      *http.Client
	readToken func() (string, error) // token do PRÓPRIO pedagio (é renovado no disco)

	mu    sync.Mutex
	cache map[[32]byte]cached
}

type cached struct {
	id      Identity
	expires time.Time
}

// NewKubernetesInCluster configura a partir do ambiente do pod.
func NewKubernetesInCluster(audience string) (*Kubernetes, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, fmt.Errorf("auth kubernetes: KUBERNETES_SERVICE_HOST vazio (rodando fora do cluster? use auth.modes: [tokens])")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("auth kubernetes: lendo CA: %w", err)
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
	if tok == "" {
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
		return Identity{}, fmt.Errorf("auth kubernetes: token próprio: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+own)
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.http.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("auth kubernetes: TokenReview: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("auth kubernetes: TokenReview HTTP %d (falta a ClusterRole system:auth-delegator?)", resp.StatusCode)
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
		return Identity{}, fmt.Errorf("auth kubernetes: resposta: %w", err)
	}
	if !out.Status.Authenticated {
		return Identity{}, ErrInvalid
	}
	// username = system:serviceaccount:<namespace>:<nome>
	const prefix = "system:serviceaccount:"
	u := out.Status.User.Username
	if !strings.HasPrefix(u, prefix) {
		return Identity{}, ErrInvalid // só ServiceAccounts, não usuários humanos
	}
	return Identity{Agent: strings.Replace(strings.TrimPrefix(u, prefix), ":", "/", 1), Method: "kubernetes"}, nil
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

// toCache evita um TokenReview por chamada de LLM. TTL curto (1 min) para
// que uma ServiceAccount removida perca acesso rápido.
func (k *Kubernetes) toCache(key [32]byte, id Identity) {
	ttl := k.TTL
	if ttl == 0 {
		ttl = time.Minute
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cache == nil || len(k.cache) > 10_000 {
		k.cache = map[[32]byte]cached{} // limpeza simples: zera tudo
	}
	k.cache[key] = cached{id: id, expires: time.Now().Add(ttl)}
}
