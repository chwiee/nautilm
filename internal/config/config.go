// Package config lê o arquivo YAML do pedagio.
//
// A ideia central: agentes pedem por APELIDO ("smart", "fast"), nunca pelo
// ID do modelo. O apelido aponta para provider+modelo aqui. Trocar Claude
// por Nova, ou Bedrock por outro provedor, é mudar este arquivo — nenhum
// agente é alterado.
//
// Segredos (API keys) NUNCA ficam no arquivo: use ${VAR} e variável de ambiente.
package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Listen    string              `yaml:"listen"`
	Providers map[string]Provider `yaml:"providers"`
	Models    map[string]Model    `yaml:"models"`
	Auth      Auth                `yaml:"auth"`
	// Agents diz que apelidos cada agente pode usar. A chave é a identidade
	// (ex: "observability/kubets" para ServiceAccount, ou o nome do token).
	// A chave "*" vale para qualquer agente autenticado que não esteja listado.
	Agents map[string]Agent `yaml:"agents"`
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
	// Type: "chat" (padrão) ou "embedding". Um apelido de embedding só
	// serve em /v1/embeddings e vice-versa.
	Type     string `yaml:"type"`
	Provider string `yaml:"provider"`
	// Dimensions padrão para embeddings (0 = padrão do modelo).
	Dimensions int    `yaml:"dimensions"`
	Model      string `yaml:"model"` // ID real no provedor
	// MaxTokens padrão quando o agente não informa (controle de custo).
	MaxTokens int `yaml:"max_tokens"`
	// Fallback: apelidos tentados, em ordem, se este falhar com 429/5xx.
	Fallback []string `yaml:"fallback"`
}

type Auth struct {
	// Modes em ordem de tentativa: "kubernetes", "tokens", "none".
	// "none" é só para desenvolvimento local.
	Modes []string `yaml:"modes"`
	// Audience que o token projetado da ServiceAccount precisa ter.
	Audience string `yaml:"audience"`
	// Tokens: nome do agente → token. Use ${VAR}.
	Tokens map[string]string `yaml:"tokens"`
}

type Agent struct {
	Models []string `yaml:"models"` // apelidos permitidos; "*" = todos
}

// Load lê o arquivo, expande ${VAR} e valida.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(b))), &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Auth.Audience == "" {
		c.Auth.Audience = "pedagio"
	}
	if len(c.Auth.Modes) == 0 {
		c.Auth.Modes = []string{"kubernetes"}
	}
	for alias, m := range c.Models {
		if m.Type == "" {
			m.Type = "chat"
			c.Models[alias] = m
		}
	}
	return &c, c.Validate()
}

// Validate pega erros de config na subida, não no meio da madrugada.
func (c *Config) Validate() error {
	var errs []error
	if len(c.Models) == 0 {
		errs = append(errs, errors.New("nenhum apelido em models"))
	}
	for name, p := range c.Providers {
		switch p.Type {
		case "bedrock":
		case "openai":
			if p.BaseURL == "" {
				errs = append(errs, fmt.Errorf("provider %s: base_url obrigatório", name))
			}
		default:
			errs = append(errs, fmt.Errorf("provider %s: type %q desconhecido (use bedrock ou openai)", name, p.Type))
		}
	}
	for alias, m := range c.Models {
		switch m.Type {
		case "chat", "embedding":
		default:
			errs = append(errs, fmt.Errorf("model %s: type %q desconhecido (use chat ou embedding)", alias, m.Type))
		}
		// Embedding NUNCA tem fallback: cada modelo gera vetores num espaço
		// diferente. Misturar dois no mesmo índice estraga a busca em silêncio.
		if m.Type == "embedding" && len(m.Fallback) > 0 {
			errs = append(errs, fmt.Errorf("model %s: embedding não pode ter fallback (vetores de modelos diferentes não se misturam)", alias))
		}
		if _, ok := c.Providers[m.Provider]; !ok {
			errs = append(errs, fmt.Errorf("model %s: provider %q não existe", alias, m.Provider))
		}
		if m.Model == "" {
			errs = append(errs, fmt.Errorf("model %s: campo model vazio", alias))
		}
		for _, f := range m.Fallback {
			if _, ok := c.Models[f]; !ok {
				errs = append(errs, fmt.Errorf("model %s: fallback %q não existe", alias, f))
			}
			if f == alias {
				errs = append(errs, fmt.Errorf("model %s: fallback para si mesmo", alias))
			}
			if fm, ok := c.Models[f]; ok && fm.Type == "embedding" {
				errs = append(errs, fmt.Errorf("model %s: fallback %q é de embedding", alias, f))
			}
		}
	}
	for _, mode := range c.Auth.Modes {
		if !slices.Contains([]string{"kubernetes", "tokens", "none"}, mode) {
			errs = append(errs, fmt.Errorf("auth: modo %q desconhecido", mode))
		}
	}
	for agent, a := range c.Agents {
		for _, m := range a.Models {
			if _, ok := c.Models[m]; !ok && m != "*" {
				errs = append(errs, fmt.Errorf("agent %s: apelido %q não existe", agent, m))
			}
		}
	}
	return errors.Join(errs...)
}

// Allowed devolve os apelidos que a identidade pode usar (ordenados).
func (c *Config) Allowed(identity string) []string {
	a, ok := c.Agents[identity]
	if !ok {
		a, ok = c.Agents["*"]
	}
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

// Chain devolve o apelido pedido seguido dos fallbacks (sem repetir).
// Fallback é de um nível só, de propósito: previsível e fácil de explicar.
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
