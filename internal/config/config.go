// Package config reads the nautilm YAML file.
//
// The central idea: agents ask for an ALIAS ("smart", "fast"), never for a
// model ID. The alias points to provider+model here. Switching Claude for
// Nova, or Bedrock for another provider, means editing this file; no agent
// changes.
//
// Secrets (API keys, tokens) NEVER live in the file: use ${VAR} and an
// environment variable.
//
// The file is read in STRICT mode: an unknown key (a typo, or an old key
// from pedagio) stops the startup with a clear error instead of being
// silently ignored.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/chwiee/nautilm/internal/mask"
	"github.com/chwiee/nautilm/internal/pricing"
)

type Config struct {
	Listen    string              `yaml:"listen"`
	Providers map[string]Provider `yaml:"providers"`
	Models    map[string]Model    `yaml:"models"`
	// Pricing is the price catalog per provider+model, versioned by date.
	Pricing []pricing.Entry `yaml:"pricing"`
	// StrictPricing makes a model without a price a startup error (instead
	// of a warning). Recommended in production.
	StrictPricing bool `yaml:"strict_pricing"`
	Auth          Auth `yaml:"auth"`
	// Agents tells which aliases each agent may use, its team/cost center
	// and quotas. The key is the identity ("observability/kubets" for a
	// ServiceAccount, or the token name). "*" applies to any authenticated
	// agent that is not listed.
	Agents map[string]Agent `yaml:"agents"`
	// Masking removes sensitive data (CPF, tokens, passwords...) before any
	// model. Absent = ON with every built-in rule.
	Masking *mask.Config `yaml:"masking"`
	// Quotas of tokens per agent. Absent = ON with the defaults below.
	Quotas Quotas `yaml:"quotas"`
	// Usage is the ledger (who used what, where, how much it cost).
	Usage Usage `yaml:"usage"`
	// Dashboard protects /dashboard data and /v1/usage.
	Dashboard Dashboard `yaml:"dashboard"`
	// ValkeyURL is shared by quotas and the usage ledger. Empty = in memory
	// (single replica, dev). With several replicas, use Valkey so every
	// replica sees the same numbers.
	ValkeyURL string `yaml:"valkey_url"`

	catalog *pricing.Catalog
}

type Provider struct {
	Type string `yaml:"type"` // "bedrock" | "openai"
	// bedrock
	Region string `yaml:"region"`
	// openai
	BaseURL string        `yaml:"base_url"`
	APIKey  string        `yaml:"api_key"` // use ${VAR}
	Timeout time.Duration `yaml:"timeout"`
}

type Model struct {
	// Type: "chat" (default) or "embedding". An embedding alias only works
	// on /v1/embeddings and vice versa.
	Type     string `yaml:"type"`
	Provider string `yaml:"provider"`
	// Dimensions default for embeddings (0 = model default).
	Dimensions int    `yaml:"dimensions"`
	Model      string `yaml:"model"` // real ID at the provider
	// MaxTokens default when the agent does not send one (cost control).
	MaxTokens int `yaml:"max_tokens"`
	// Fallback: aliases tried, in order, if this one fails with 429/5xx.
	Fallback []string `yaml:"fallback"`
}

type Auth struct {
	// Modes in the order they are tried: "kubernetes", "tokens", "none".
	// "none" is for local development only.
	Modes []string `yaml:"modes"`
	// Audience the projected ServiceAccount token must carry.
	Audience string `yaml:"audience"`
	// Tokens: agent name → token. Use ${VAR}.
	Tokens map[string]string `yaml:"tokens"`
}

type Agent struct {
	Models []string `yaml:"models"` // allowed aliases; "*" = all
	// Team and CostCenter go into every usage record: the bill can be split
	// by team, not only by agent.
	Team       string `yaml:"team"`
	CostCenter string `yaml:"cost_center"`
	// OnBehalfOf lets this agent (e.g. the orchestrator) bill a call to the
	// agent it is serving, with the X-Nautilm-On-Behalf-Of header. Without
	// it the header is ignored: otherwise any agent could push its bill to
	// another one.
	OnBehalfOf bool `yaml:"on_behalf_of"`
	// Own quota (0 = use quotas.*, -1 = no limit).
	TokensPerMinute int64 `yaml:"tokens_per_minute"`
	TokensPerDay    int64 `yaml:"tokens_per_day"`
}

// Quota defaults: generous for normal use, but they stop an agent in a loop.
const (
	DefaultTokensPerMinute = 200_000
	DefaultTokensPerDay    = 10_000_000
	DefaultRetentionDays   = 400
)

type Quotas struct {
	Disabled bool `yaml:"disabled"`
	// Default limits of EVERY agent (0 = nautilm default, -1 = no limit).
	TokensPerMinute int64 `yaml:"tokens_per_minute"`
	TokensPerDay    int64 `yaml:"tokens_per_day"`
}

type Usage struct {
	Disabled bool `yaml:"disabled"`
	// RetentionDays of the daily totals (default 400: a year plus a margin
	// to compare the same month of the previous year).
	RetentionDays int `yaml:"retention_days"`
}

type Dashboard struct {
	// Token for /v1/usage and the dashboard data. Use ${VAR}. Empty = the
	// usage API is disabled (cost data is internal: it is not open by default).
	Token string `yaml:"token"`
}

// Load reads the file, expands ${VAR} and validates.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// legacy maps old pedagio keys to the new ones (better error messages).
var legacy = map[string]string{"mascarar": "masking", "cotas": "quotas", "preco": "pricing (per provider+model)",
	"desligado": "disabled", "desligar": "disable", "tokens_por_minuto": "tokens_per_minute", "tokens_por_dia": "tokens_per_day"}

var fieldNotFound = regexp.MustCompile(`field (\w+) not found`)

func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader([]byte(os.ExpandEnv(string(b)))))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		msg := err.Error()
		if m := fieldNotFound.FindStringSubmatch(msg); m != nil {
			if repl, ok := legacy[m[1]]; ok {
				msg += fmt.Sprintf(" (old pedagio key: use %q)", repl)
			}
		}
		return nil, fmt.Errorf("config: %s", msg)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Auth.Audience == "" {
		c.Auth.Audience = "nautilm"
	}
	if len(c.Auth.Modes) == 0 {
		c.Auth.Modes = []string{"kubernetes"}
	}
	if c.Usage.RetentionDays == 0 {
		c.Usage.RetentionDays = DefaultRetentionDays
	}
	for alias, m := range c.Models {
		if m.Type == "" {
			m.Type = "chat"
			c.Models[alias] = m
		}
	}
	return &c, c.Validate()
}

// Validate catches config errors at startup, not in the middle of the night.
func (c *Config) Validate() error {
	var errs []error
	if len(c.Models) == 0 {
		errs = append(errs, errors.New("no alias in models"))
	}
	for name, p := range c.Providers {
		switch p.Type {
		case "bedrock":
		case "openai":
			if p.BaseURL == "" {
				errs = append(errs, fmt.Errorf("provider %s: base_url is required", name))
			}
		default:
			errs = append(errs, fmt.Errorf("provider %s: unknown type %q (use bedrock or openai)", name, p.Type))
		}
	}
	for alias, m := range c.Models {
		switch m.Type {
		case "chat", "embedding":
		default:
			errs = append(errs, fmt.Errorf("model %s: unknown type %q (use chat or embedding)", alias, m.Type))
		}
		// Embeddings NEVER have a fallback: each model builds vectors in a
		// different space. Mixing two in one index silently breaks search.
		if m.Type == "embedding" && len(m.Fallback) > 0 {
			errs = append(errs, fmt.Errorf("model %s: an embedding cannot have a fallback (vectors from different models do not mix)", alias))
		}
		if _, ok := c.Providers[m.Provider]; !ok {
			errs = append(errs, fmt.Errorf("model %s: provider %q does not exist", alias, m.Provider))
		}
		if m.Model == "" {
			errs = append(errs, fmt.Errorf("model %s: empty model field", alias))
		}
		for _, f := range m.Fallback {
			if _, ok := c.Models[f]; !ok {
				errs = append(errs, fmt.Errorf("model %s: fallback %q does not exist", alias, f))
			}
			if f == alias {
				errs = append(errs, fmt.Errorf("model %s: fallback to itself", alias))
			}
			if fm, ok := c.Models[f]; ok && fm.Type == "embedding" {
				errs = append(errs, fmt.Errorf("model %s: fallback %q is an embedding", alias, f))
			}
		}
	}
	for _, mode := range c.Auth.Modes {
		if !slices.Contains([]string{"kubernetes", "tokens", "none"}, mode) {
			errs = append(errs, fmt.Errorf("auth: unknown mode %q", mode))
		}
	}
	for agent, a := range c.Agents {
		for _, m := range a.Models {
			if _, ok := c.Models[m]; !ok && m != "*" {
				errs = append(errs, fmt.Errorf("agent %s: alias %q does not exist", agent, m))
			}
		}
	}
	if _, err := mask.New(c.Masking); err != nil {
		errs = append(errs, err)
	}
	for i, p := range c.Pricing {
		if _, ok := c.Providers[p.Provider]; !ok && p.Provider != "" {
			errs = append(errs, fmt.Errorf("pricing[%d]: provider %q does not exist", i, p.Provider))
		}
	}
	cat, err := pricing.New(c.Pricing)
	if err != nil {
		errs = append(errs, err)
	}
	c.catalog = cat
	if err == nil && c.StrictPricing {
		if missing := c.MissingPrices(time.Now()); len(missing) > 0 {
			errs = append(errs, fmt.Errorf("strict_pricing: no price in effect for %s", strings.Join(missing, ", ")))
		}
	}
	if c.Usage.RetentionDays < 0 {
		errs = append(errs, errors.New("usage.retention_days cannot be negative"))
	}
	return errors.Join(errs...)
}

// Catalog returns the price catalog (validated).
func (c *Config) Catalog() *pricing.Catalog { return c.catalog }

// MissingPrices lists provider/model pairs used by aliases with no price in
// effect at "at". Their calls are recorded as "unpriced" (cost UNKNOWN).
func (c *Config) MissingPrices(at time.Time) []string {
	seen := map[[2]string]bool{}
	var pairs [][2]string
	for _, m := range c.Models {
		p := [2]string{m.Provider, m.Model}
		if !seen[p] {
			seen[p] = true
			pairs = append(pairs, p)
		}
	}
	return c.catalog.Missing(pairs, at)
}

// agent returns the agent entry for an identity: its own, else "*".
func (c *Config) agent(identity string) (Agent, bool) {
	if a, ok := c.Agents[identity]; ok {
		return a, true
	}
	a, ok := c.Agents["*"]
	return a, ok
}

// Allowed returns the aliases the identity may use (sorted).
func (c *Config) Allowed(identity string) []string {
	a, ok := c.agent(identity)
	if !ok {
		return nil
	}
	var out []string
	if slices.Contains(a.Models, "*") {
		for alias := range c.Models {
			out = append(out, alias)
		}
	} else {
		out = append(out, a.Models...)
	}
	sort.Strings(out)
	return out
}

// Billing returns the team and cost center of an agent (for usage records).
func (c *Config) Billing(identity string) (team, costCenter string) {
	a, _ := c.agent(identity)
	return a.Team, a.CostCenter
}

// CanActOnBehalf tells whether the identity may bill calls to another agent.
// Only an explicit entry counts ("*" never grants it).
func (c *Config) CanActOnBehalf(identity string) bool {
	return c.Agents[identity].OnBehalfOf
}

// Chain returns the requested alias followed by its fallbacks (no repeats).
// Fallback is one level only, on purpose: predictable and easy to explain.
func (c *Config) Chain(alias string) []string {
	m, ok := c.Models[alias]
	if !ok {
		return nil
	}
	chain := []string{alias}
	for _, f := range m.Fallback {
		if !slices.Contains(chain, f) {
			chain = append(chain, f)
		}
	}
	return chain
}

// Limits returns the effective quota of the identity: its own, else the
// "*" one (only for agents that are not listed, same rule as "agents"),
// else the global one, else the default. A result <= 0 = no limit.
func (c *Config) Limits(identity string) (perMinute, perDay int64) {
	if c.Quotas.Disabled {
		return 0, 0
	}
	pick := func(agent, star, global, def int64) int64 {
		for _, v := range []int64{agent, star, global} {
			if v != 0 {
				return max(v, 0) // -1 becomes 0 = no limit
			}
		}
		return def
	}
	a, listed := c.Agents[identity]
	var s Agent
	if !listed {
		s = c.Agents["*"]
	}
	return pick(a.TokensPerMinute, s.TokensPerMinute, c.Quotas.TokensPerMinute, DefaultTokensPerMinute),
		pick(a.TokensPerDay, s.TokensPerDay, c.Quotas.TokensPerDay, DefaultTokensPerDay)
}
