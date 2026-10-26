// Package mask removes sensitive data from text BEFORE it reaches a model.
//
// Why in nautilm? Because every agent goes through it: one rule, valid for
// everyone, without depending on each team remembering. What the model
// never sees, it cannot leak (not to the provider, not in the answer, not in
// the logs of whoever looks at the prompt).
//
// On by default with the built-in rules below. Tune it in YAML:
//
//	masking:
//	  disable: [email]                           # turn built-in rules off
//	  extra:                                      # add your own
//	    - {name: employee_id, regex: 'MAT-\d{6}'}
//	# or: masking: {disabled: true}
package mask

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Rule replaces what matches Re with Replace (may use $1 to keep groups).
type Rule struct {
	Name    string
	Re      *regexp.Regexp
	Replace string
	// Check is optional: only mask when it returns true (e.g. Luhn for cards).
	Check func(match string) bool
	// Fn is optional: builds the replacement from the groups (when Replace is not enough).
	Fn func(groups []string) string
}

// Builtin returns the built-in rules, in the order they run (most specific
// first: a private key contains things that would look like a token).
func Builtin() []Rule {
	return []Rule{
		{Name: "private_key", Re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), Replace: "[PRIVATE_KEY]"},
		{Name: "jwt", Re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), Replace: "[JWT]"},
		{Name: "aws_key", Re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), Replace: "[AWS_KEY]"},
		{Name: "bearer", Re: regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{16,}`), Replace: "$1 [TOKEN]"},
		// key=value, key: value and "key":"value" (tool-call JSON arguments) for
		// names that mean a secret, with prefixes (DB_PASSWORD, GITHUB_TOKEN).
		// "senha" is Portuguese for password: it shows up in our logs.
		// Quotes are kept, so the JSON stays valid.
		{Name: "secret", Re: regexp.MustCompile(`(?i)\b([A-Za-z0-9_-]*(?:senha|password|passwd|pwd|secret|api[_-]?key|token))("?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;"'}]+)`), Fn: func(g []string) string {
			q := ""
			if c := g[3][0]; c == '"' || c == '\'' {
				q = string(c)
			}
			return g[1] + g[2] + q + "[SECRET]" + q
		}},
		{Name: "cpf", Re: regexp.MustCompile(`\b\d{3}\.\d{3}\.\d{3}-\d{2}\b`), Replace: "[CPF]"},
		{Name: "cnpj", Re: regexp.MustCompile(`\b\d{2}\.\d{3}\.\d{3}/\d{4}-\d{2}\b`), Replace: "[CNPJ]"},
		{Name: "credit_card", Re: regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`), Replace: "[CREDIT_CARD]", Check: luhn},
		{Name: "email", Re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), Replace: "[EMAIL]"},
	}
}

// Config is the "masking" block of the YAML.
type Config struct {
	Disabled bool     `yaml:"disabled"`
	Disable  []string `yaml:"disable"`
	Extra    []Extra  `yaml:"extra"`
}

type Extra struct {
	Name  string `yaml:"name"`
	Regex string `yaml:"regex"`
}

// Masker applies the rules.
type Masker struct{ rules []Rule }

// New builds the Masker. A nil cfg = all built-in rules (on by default).
func New(cfg *Config) (*Masker, error) {
	if cfg != nil && cfg.Disabled {
		return &Masker{}, nil
	}
	var rules []Rule
	known := map[string]bool{}
	for _, r := range Builtin() {
		known[r.Name] = true
		if cfg == nil || !slices.Contains(cfg.Disable, r.Name) {
			rules = append(rules, r)
		}
	}
	if cfg != nil {
		for _, d := range cfg.Disable {
			if !known[d] {
				return nil, fmt.Errorf("masking.disable: rule %q does not exist (built-in: %s)", d, strings.Join(Names(), ", "))
			}
		}
		for _, e := range cfg.Extra {
			if e.Name == "" {
				return nil, fmt.Errorf("masking.extra: rule without a name (regex %q)", e.Regex)
			}
			re, err := regexp.Compile(e.Regex)
			if err != nil {
				return nil, fmt.Errorf("masking.extra %q: invalid regex: %w", e.Name, err)
			}
			rules = append(rules, Rule{Name: e.Name, Re: re, Replace: "[" + strings.ToUpper(e.Name) + "]"})
		}
	}
	return &Masker{rules: rules}, nil
}

// Names lists the built-in rules.
func Names() []string {
	var n []string
	for _, r := range Builtin() {
		n = append(n, r.Name)
	}
	return n
}

// Enabled lists the active rules (startup log).
func (m *Masker) Enabled() []string {
	var n []string
	for _, r := range m.rules {
		n = append(n, r.Name)
	}
	return n
}

// Apply masks the text and counts how many replacements each rule made.
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

// luhn validates a card number (avoids masking any digit sequence).
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
