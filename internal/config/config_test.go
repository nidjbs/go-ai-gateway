package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesDefaultsAndResolvesProviderSecret(t *testing.T) {
	t.Setenv("UPSTREAM_API_KEY", "test-upstream-token")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`providers:
  local:
    base_url: http://127.0.0.1:19090/v1
    api_key_env: UPSTREAM_API_KEY
aliases:
  chat:
    provider: local
    model: test-model
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, "127.0.0.1:8080")
	}
	if cfg.Auth.Mode != "none" {
		t.Errorf("Auth.Mode = %q, want %q", cfg.Auth.Mode, "none")
	}
	if cfg.Retry.MaxAttemptsPerProvider != 1 {
		t.Errorf("MaxAttemptsPerProvider = %d, want 1", cfg.Retry.MaxAttemptsPerProvider)
	}
	if cfg.Providers["local"].APIKey != "test-upstream-token" {
		t.Error("provider API key was not resolved from environment")
	}
}
