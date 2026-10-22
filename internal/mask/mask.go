// Package mask tira dados sensíveis do texto ANTES de ele ir para o modelo.
//
// Por que no pedagio? Porque todo agente passa por aqui: uma regra só,
// valendo para todos, sem depender de cada time lembrar. O que o modelo
// nunca vê, ele não vaza (nem para o provedor, nem na resposta, nem no log
// de quem estiver olhando o prompt).
//
// Ligado por padrão com as regras embutidas abaixo. Ajuste no YAML:
//
//	mascarar:
//	  desligar: [email]                          # tira regras embutidas
//	  extra:                                      # acrescenta as suas
//	    - {nome: matricula, regex: 'MAT-\d{6}'}
//	# ou: mascarar: {desligado: true}
package mask

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Rule troca o que casa com Re por Replace (pode usar $1 para manter grupos).
type Rule struct {
	Name    string
	Re      *regexp.Regexp
	Replace string
	// Check opcional: só mascara se devolver true (ex: Luhn no cartão).
	Check func(match string) bool
	// Fn opcional: monta a troca a partir dos grupos (quando Replace não basta).
	Fn func(groups []string) string
}

// Builtin são as regras embutidas, na ordem em que rodam (as mais
// específicas primeiro: uma chave privada contém coisas que pareceriam token).
func Builtin() []Rule {
	return []Rule{
		{Name: "chave_privada", Re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), Replace: "[CHAVE_PRIVADA]"},
		{Name: "jwt", Re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), Replace: "[JWT]"},
		{Name: "aws_key", Re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), Replace: "[AWS_KEY]"},
		{Name: "bearer", Re: regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{16,}`), Replace: "$1 [TOKEN]"},
		// chave=valor, chave: valor e "chave":"valor" (JSON dos argumentos de
		// tool) para nomes que indicam segredo. Mantém as aspas: o JSON continua válido.
		{Name: "segredo", Re: regexp.MustCompile(`(?i)\b([A-Za-z0-9_-]*(?:senha|password|passwd|pwd|secret|api[_-]?key|token))("?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;"'}]+)`), Fn: func(g []string) string {
			q := ""
			if c := g[3][0]; c == '"' || c == '\'' {
				q = string(c)
			}
			return g[1] + g[2] + q + "[SEGREDO]" + q
		}},
		{Name: "cpf", Re: regexp.MustCompile(`\b\d{3}\.\d{3}\.\d{3}-\d{2}\b`), Replace: "[CPF]"},
		{Name: "cnpj", Re: regexp.MustCompile(`\b\d{2}\.\d{3}\.\d{3}/\d{4}-\d{2}\b`), Replace: "[CNPJ]"},
		{Name: "cartao", Re: regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`), Replace: "[CARTAO]", Check: luhn},
		{Name: "email", Re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), Replace: "[EMAIL]"},
	}
}

// Config é o bloco "mascarar" do YAML.
type Config struct {
	Desligado bool     `yaml:"desligado"`
	Desligar  []string `yaml:"desligar"`
	Extra     []Extra  `yaml:"extra"`
}

type Extra struct {
	Nome  string `yaml:"nome"`
	Regex string `yaml:"regex"`
}

// Masker aplica as regras.
type Masker struct{ rules []Rule }

// New monta o Masker. cfg nil = todas as regras embutidas (ligado por padrão).
func New(cfg *Config) (*Masker, error) {
	if cfg != nil && cfg.Desligado {
		return &Masker{}, nil
	}
	var rules []Rule
	known := map[string]bool{}
	for _, r := range Builtin() {
		known[r.Name] = true
		if cfg == nil || !slices.Contains(cfg.Desligar, r.Name) {
			rules = append(rules, r)
		}
	}
	if cfg != nil {
		for _, d := range cfg.Desligar {
			if !known[d] {
				return nil, fmt.Errorf("mascarar.desligar: regra %q não existe (embutidas: %s)", d, strings.Join(Names(), ", "))
			}
		}
		for _, e := range cfg.Extra {
			re, err := regexp.Compile(e.Regex)
			if err != nil {
				return nil, fmt.Errorf("mascarar.extra %q: regex inválida: %w", e.Nome, err)
			}
			if e.Nome == "" {
				return nil, fmt.Errorf("mascarar.extra: regra sem nome (regex %q)", e.Regex)
			}
			rules = append(rules, Rule{Name: e.Nome, Re: re, Replace: "[" + strings.ToUpper(e.Nome) + "]"})
		}
	}
	return &Masker{rules: rules}, nil
}

// Names lista as regras embutidas.
func Names() []string {
	var n []string
	for _, r := range Builtin() {
		n = append(n, r.Name)
	}
	return n
}

// Enabled lista as regras ativas (log de subida).
func (m *Masker) Enabled() []string {
	var n []string
	for _, r := range m.rules {
		n = append(n, r.Name)
	}
	return n
}

// Apply mascara o texto e conta quantas trocas cada regra fez.
func (m *Masker) Apply(s string, counts map[string]int) string {
	if m == nil || s == "" {
		return s
	}
	for _, r := range m.rules {
		s = r.Re.ReplaceAllStringFunc(s, func(match string) string {
			if r.Check != nil && !r.Check(match) {
				return match
			}
			counts[r.Name]++
			if r.Fn != nil {
				return r.Fn(r.Re.FindStringSubmatch(match))
			}
			return r.Re.ReplaceAllString(match, r.Replace)
		})
	}
	return s
}

// luhn valida número de cartão (evita mascarar qualquer sequência de dígitos).
func luhn(s string) bool {
	var digits []int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			digits = append(digits, int(c-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if (len(digits)-1-i)%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}
