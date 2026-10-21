// Package auth descobre QUEM está chamando o pedagio.
//
// Modo recomendado no EKS: "kubernetes". O agente manda o token da própria
// ServiceAccount (projetado com audience "pedagio") e o pedagio pergunta ao
// API server se ele é válido (TokenReview). Vantagens:
//   - nenhum segredo para distribuir ou rotacionar;
//   - a identidade é "namespace/serviceaccount", que já diz o time/agente;
//   - o token expira sozinho (o kubelet renova).
//
// "tokens" serve para quem está fora do cluster (CLI, laboratório).
// "none" é só para desenvolvimento local.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// Identity é quem está chamando.
type Identity struct {
	Agent  string // ex: "observability/kubets" ou "ce-cli"
	Method string // kubernetes | tokens | none
}

var (
	// ErrNoCredentials: este autenticador não se aplica (tenta o próximo).
	ErrNoCredentials = errors.New("sem credenciais")
	// ErrInvalid: credencial presente, mas inválida (para tudo: 401).
	ErrInvalid = errors.New("credencial inválida")
)

type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// Chain tenta os autenticadores em ordem.
type Chain []Authenticator

func (c Chain) Authenticate(r *http.Request) (Identity, error) {
	for _, a := range c {
		id, err := a.Authenticate(r)
		if errors.Is(err, ErrNoCredentials) {
			continue
		}
		return id, err
	}
	return Identity{}, ErrNoCredentials
}

// BearerToken extrai o token do header Authorization.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Tokens autentica por tokens estáticos (nome → token).
type Tokens struct {
	byHash map[[32]byte]string
}

func NewTokens(m map[string]string) *Tokens {
	t := &Tokens{byHash: map[[32]byte]string{}}
	for name, tok := range m {
		if tok != "" {
			t.byHash[sha256.Sum256([]byte(tok))] = name
		}
	}
	return t
}

func (t *Tokens) Authenticate(r *http.Request) (Identity, error) {
	tok := BearerToken(r)
	if tok == "" {
		return Identity{}, ErrNoCredentials
	}
	h := sha256.Sum256([]byte(tok))
	// Compara pelo hash em tempo constante, para não vazar timing.
	for k, name := range t.byHash {
		if subtle.ConstantTimeCompare(k[:], h[:]) == 1 {
			return Identity{Agent: name, Method: "tokens"}, nil
		}
	}
	// Pode ser um token de ServiceAccount: deixa o próximo tentar.
	return Identity{}, ErrNoCredentials
}

// None aceita todo mundo. Agente vem do header X-Pedagio-Agent.
// NUNCA use em produção.
type None struct{}

func (None) Authenticate(r *http.Request) (Identity, error) {
	name := r.Header.Get("X-Pedagio-Agent")
	if name == "" {
		name = "anonimo"
	}
	return Identity{Agent: name, Method: "none"}, nil
}
