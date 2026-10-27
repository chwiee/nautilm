package live

import (
	"context"
	"fmt"

	"github.com/chwiee/nautilm/internal/config"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/internal/provider/bedrock"
	"github.com/chwiee/nautilm/internal/provider/openaicompat"
)

// DefaultBuilder creates the real clients (Bedrock SDK or OpenAI-compatible HTTP).
func DefaultBuilder(ctx context.Context, name string, p config.Provider) (provider.Provider, error) {
	switch p.Type {
	case "bedrock":
		b, err := bedrock.New(ctx, bedrock.Config{Region: p.Region})
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		return b, nil
	case "openai":
		return openaicompat.New(openaicompat.Config{BaseURL: p.BaseURL, APIKey: p.APIKey, Timeout: p.Timeout}), nil
	}
	return nil, fmt.Errorf("provider %s: unknown type %q", name, p.Type)
}

// BuildAll creates the clients of every provider in the config file.
func BuildAll(ctx context.Context, cfg *config.Config, build Builder) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	for name, p := range cfg.Providers {
		c, err := build(ctx, name, p)
		if err != nil {
			return nil, err
		}
		out[name] = c
	}
	return out, nil
}
