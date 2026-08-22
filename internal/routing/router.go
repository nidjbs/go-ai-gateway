package routing

import (
	"fmt"
	"sort"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

type Candidate struct {
	Name     string
	Type     string
	Model    string
	BaseURL  string
	APIKey   string
	Timeout  time.Duration
	Priority int
	// ForceUsage mirrors the provider's force_usage setting (nil = unset).
	ForceUsage *bool
}

func Resolve(cfg *config.Config, alias string) ([]Candidate, error) {
	definition, ok := cfg.Aliases[alias]
	if !ok {
		return nil, fmt.Errorf("alias %q is not defined", alias)
	}
	raw := definition.NormalizedProviders()
	out := make([]Candidate, 0, len(raw))
	for _, item := range raw {
		provider, ok := cfg.Providers[item.Name]
		if !ok {
			return nil, fmt.Errorf("alias %q references unknown provider %q", alias, item.Name)
		}
		providerType := provider.Type
		if providerType == "" {
			providerType = "openai"
		}
		out = append(out, Candidate{Name: item.Name, Type: providerType, Model: item.Model, BaseURL: provider.BaseURL, APIKey: provider.APIKey, Timeout: provider.RequestTimeout, Priority: item.Priority, ForceUsage: provider.ForceUsage})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}
