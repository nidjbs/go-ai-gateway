package routing

import (
	"testing"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

func TestResolveSortsProvidersByPriority(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"primary": {BaseURL: "http://primary", APIKey: "a"},
			"backup":  {BaseURL: "http://backup", APIKey: "b"},
		},
		Aliases: map[string]config.Alias{"chat": {Providers: []config.AliasProvider{
			{Name: "backup", Model: "backup-model", Priority: 10},
			{Name: "primary", Model: "primary-model", Priority: 0},
		}}},
	}

	candidates, err := Resolve(cfg, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].Name != "primary" || candidates[1].Name != "backup" {
		t.Fatalf("unexpected candidates: %#v", candidates)
	}
}

func TestResolveCarriesForceUsageFromProvider(t *testing.T) {
	force := true
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"primary": {BaseURL: "http://primary", APIKey: "a", ForceUsage: &force},
			"backup":  {BaseURL: "http://backup", APIKey: "b"},
		},
		Aliases: map[string]config.Alias{"chat": {Providers: []config.AliasProvider{
			{Name: "backup", Model: "b", Priority: 1},
			{Name: "primary", Model: "a", Priority: 0},
		}}},
	}
	candidates, err := Resolve(cfg, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if candidates[0].ForceUsage == nil || !*candidates[0].ForceUsage {
		t.Fatalf("primary force_usage not carried: %#v", candidates[0])
	}
	if candidates[1].ForceUsage != nil {
		t.Fatalf("unset provider must stay nil: %#v", candidates[1])
	}
}
