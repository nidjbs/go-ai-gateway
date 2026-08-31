package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if !cfg.Retry.IsEnabled() || !cfg.Failover.IsEnabled() {
		t.Error("retry and failover should default to enabled")
	}
	if cfg.Retry.InitialInterval != 200*time.Millisecond || cfg.Retry.MaxInterval != 5*time.Second || cfg.Retry.Multiplier != 2 || cfg.Retry.Jitter != 0.2 {
		t.Errorf("unexpected retry defaults: %+v", cfg.Retry)
	}
	if cfg.Providers["local"].APIKey != "test-upstream-token" {
		t.Error("provider API key was not resolved from environment")
	}
}

func TestLoadHonorsDisabledRetryAndFailover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`providers:
  local:
    base_url: http://127.0.0.1:19090/v1
aliases:
  chat:
    provider: local
    model: test-model
retry:
  enabled: false
failover:
  enabled: false
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retry.IsEnabled() {
		t.Error("retry should be disabled")
	}
	if cfg.Failover.IsEnabled() {
		t.Error("failover should be disabled")
	}
}

func TestLoadSupportsAPIKeyTeams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`auth:
  mode: api-key
providers:
  local:
    base_url: http://127.0.0.1:19090/v1
aliases:
  chat:
    provider: local
    model: mock-chat
teams:
  - id: team-a
    name: Team A
    api_keys:
      - id: key-a
        key: sk-abcdefghijklmnopqrstuvwxyz123456
        limits:
          rps: 10
          burst: 20
          preday_tokens: 5000
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Mode != "api-key" {
		t.Fatalf("Auth.Mode = %q, want api-key", cfg.Auth.Mode)
	}
	if len(cfg.Teams) != 1 || len(cfg.Teams[0].APIKeys) != 1 {
		t.Fatalf("unexpected teams: %+v", cfg.Teams)
	}
	if cfg.Teams[0].APIKeys[0].Limits.Burst != 20 {
		t.Fatalf("Burst = %d, want 20", cfg.Teams[0].APIKeys[0].Limits.Burst)
	}
}

func TestLoadRejectsDuplicateAPIKeysAcrossTeams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`auth:
  mode: api-key
providers:
  local:
    base_url: http://127.0.0.1:19090/v1
aliases:
  chat:
    provider: local
    model: mock-chat
teams:
  - id: team-a
    api_keys:
      - id: key-a
        key: sk-duplicate-key-1234567890
  - id: team-b
    api_keys:
      - id: key-b
        key: sk-duplicate-key-1234567890
`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate api key") {
		t.Fatalf("err = %v, want duplicate api key error", err)
	}
}

func TestLoadRejectsAPIKeyWithoutTeams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`auth:
  mode: api-key
providers:
  local:
    base_url: http://127.0.0.1:19090/v1
aliases:
  chat:
    provider: local
    model: mock-chat
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "no teams") {
		t.Fatalf("err = %v, want no teams error", err)
	}
}

func TestLoadDefaultsBurstToRPS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`auth:
  mode: api-key
providers:
  local:
    base_url: http://127.0.0.1:19090/v1
aliases:
  chat:
    provider: local
    model: mock-chat
teams:
  - id: team-a
    api_keys:
      - id: key-a
        key: sk-default-burst-1234567890
        limits:
          rps: 7
          preday_tokens: 100
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Teams[0].APIKeys[0].Limits.Burst != 7 {
		t.Fatalf("Burst = %d, want 7", cfg.Teams[0].APIKeys[0].Limits.Burst)
	}
}

func TestLoadAppliesGuardrailsDefaultsWhenEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`providers:
  local:
    base_url: http://127.0.0.1:19090/v1
aliases:
  chat:
    provider: local
    model: test-model
guardrails:
  enabled: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Guardrails.Mode != "flag" {
		t.Errorf("Guardrails.Mode = %q, want %q (enabled without mode must not silently disable)", cfg.Guardrails.Mode, "flag")
	}
	if cfg.Guardrails.Threshold != 0.75 {
		t.Errorf("Guardrails.Threshold = %v, want 0.75", cfg.Guardrails.Threshold)
	}
	if cfg.Guardrails.Tracker.MaxAttempts != 3 || cfg.Guardrails.Tracker.WindowSec != 60 || cfg.Guardrails.Tracker.PenaltySec != 30 {
		t.Errorf("unexpected tracker defaults: %+v", cfg.Guardrails.Tracker)
	}
}

func TestLoadRejectsInvalidGuardrailsModeAndThreshold(t *testing.T) {
	for name, extra := range map[string]string{
		"bad mode":      "  mode: nuke\n",
		"bad threshold": "  mode: flag\n  threshold: 1.5\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(`providers:
  local:
    base_url: http://127.0.0.1:19090/v1
aliases:
  chat:
    provider: local
    model: test-model
guardrails:
  enabled: true
`+extra), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("expected config error for guardrails %q", name)
			}
		})
	}
}

func TestLoadParsesAliasStrategyAndWeights(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`providers:
  local:
    base_url: http://127.0.0.1:19090/v1
  backup:
    base_url: http://127.0.0.2:19090/v1
aliases:
  chat:
    strategy: loadbalance
    providers:
      - {name: local, model: model-a, priority: 0, weight: 3}
      - {name: backup, model: model-b, priority: 1, weight: 1}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Aliases["chat"].Strategy; got != "loadbalance" {
		t.Errorf("Strategy = %q, want loadbalance", got)
	}
	providers := cfg.Aliases["chat"].NormalizedProviders()
	if len(providers) != 2 {
		t.Fatalf("len(providers) = %d, want 2", len(providers))
	}
	if providers[0].Weight != 3 || providers[1].Weight != 1 {
		t.Errorf("weights = (%d, %d), want (3, 1)", providers[0].Weight, providers[1].Weight)
	}
}

func TestLoadRejectsInvalidAliasStrategyAndWeight(t *testing.T) {
	for name, extra := range map[string]string{
		"bad strategy":    "  strategy: roundrobin\n  provider: local\n  model: test-model\n",
		"negative weight": "  provider: local\n  model: test-model\n  providers:\n    - {name: local, model: m, weight: -1}\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(`providers:
  local:
    base_url: http://127.0.0.1:19090/v1
aliases:
  chat:
`+extra), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("expected config error for alias %q", name)
			}
		})
	}
}

func TestLoadDefaultsAndHonorsMaxRequestBodyBytes(t *testing.T) {
	base := `providers:
  local:
    base_url: http://127.0.0.1:19090/v1
aliases:
  chat:
    provider: local
    model: test-model
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.MaxRequestBodyBytes != 1<<20 {
		t.Errorf("default MaxRequestBodyBytes = %d, want %d", cfg.Server.MaxRequestBodyBytes, 1<<20)
	}

	path = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(base+"server:\n  max_request_body_bytes: 8388608\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.MaxRequestBodyBytes != 8<<20 {
		t.Errorf("explicit MaxRequestBodyBytes = %d, want %d", cfg.Server.MaxRequestBodyBytes, 8<<20)
	}
}

func TestLoadRejectsInvalidTracingSampleRatio(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`providers:
  local:
    base_url: http://127.0.0.1:19090/v1
aliases:
  chat:
    provider: local
    model: test-model
tracing:
  enabled: true
  sample_ratio: 2.0
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for tracing.sample_ratio > 1")
	}
}

func TestValidateEvents(t *testing.T) {
	cfg := Config{}
	if err := cfg.validateEvents(); err != nil {
		t.Fatalf("empty events should validate: %v", err)
	}
	cfg.Events = EventsConfig{Webhooks: []WebhookConfig{{URL: "https://x.com"}}}
	if err := cfg.validateEvents(); err == nil {
		t.Fatal("missing webhook name must fail")
	}
	cfg.Events = EventsConfig{Webhooks: []WebhookConfig{{Name: "t", URL: "not-a-url"}}}
	if err := cfg.validateEvents(); err == nil {
		t.Fatal("bad webhook url must fail")
	}
	cfg.Events = EventsConfig{Webhooks: []WebhookConfig{{Name: "t", URL: "ftp://x.com"}}}
	if err := cfg.validateEvents(); err == nil {
		t.Fatal("non-http scheme must fail")
	}
}
