package config

import (
	"strings"
	"testing"
)

func TestExemploEValido(t *testing.T) {
	if _, err := Load("../../config.example.yaml"); err != nil {
		t.Fatalf("config.example.yaml precisa continuar válido: %v", err)
	}
}

func TestValidaReferencias(t *testing.T) {
	_, err := Parse([]byte(`
providers: {p: {type: openai, base_url: http://x}}
models:
  a: {provider: nao-existe, model: m, fallback: [b]}
agents:
  x: {models: [c]}
`))
	for _, want := range []string{`provider "nao-existe"`, `fallback "b"`, `apelido "c"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("esperava erro contendo %s, veio: %v", want, err)
		}
	}
}

func TestExpandeVariavel(t *testing.T) {
	t.Setenv("TOK", "abc")
	c, err := Parse([]byte(`
providers: {p: {type: openai, base_url: http://x}}
models: {a: {provider: p, model: m}}
auth: {modes: [tokens], tokens: {cli: "${TOK}"}}
`))
	if err != nil || c.Auth.Tokens["cli"] != "abc" {
		t.Fatalf("err=%v tokens=%v", err, c.Auth.Tokens)
	}
}
