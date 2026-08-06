package routing

import (
	"testing"

	"example.com/light-llm-gateway/internal/config"
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
