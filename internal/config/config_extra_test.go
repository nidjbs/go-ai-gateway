package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestNewProductionFieldsParse(t *testing.T) {
	data := []byte(`listen: 127.0.0.1:8080
healthz: 127.0.0.1:8081
ops_token_env: OPS_TOKEN
auth:
  mode: api-key
teams:
  - id: team-a
    api_keys:
      - id: key-a
        key: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        limits:
          max_requests_per_day: 5
          max_tokens_per_request: 100
providers:
  p:
    base_url: http://localhost:1
aliases:
  chat:
    provider: p
    model: m
server:
  stream_idle_timeout: 2m
  stream_max_duration: 15m
  read_timeout: 45s
  idle_timeout: 120s
  idempotency_enabled: true
  idempotency_ttl: 2h
guardrails:
  enabled: true
  mode: block
  allowlist:
    - BENCHMARK_PAYLOAD
    - RED_TEAM
`)

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults()

	if cfg.OpsTokenEnv != "OPS_TOKEN" {
		t.Fatalf("ops_token_env = %q", cfg.OpsTokenEnv)
	}
	lim := cfg.Teams[0].APIKeys[0].Limits
	if lim.MaxRequestsPerDay != 5 || lim.MaxTokensPerRequest != 100 {
		t.Fatalf("limits = %+v", lim)
	}
	s := cfg.Server
	if s.StreamIdleTimeout != 2*time.Minute || s.StreamMaxDuration != 15*time.Minute {
		t.Fatalf("stream timeouts = %+v", s)
	}
	if s.ReadTimeout != 45*time.Second || s.IdleTimeout != 120*time.Second {
		t.Fatalf("http timeouts = %+v", s)
	}
	if !s.IdempotencyEnabled || s.IdempotencyTTL != 2*time.Hour {
		t.Fatalf("idempotency = %+v", s)
	}
	if len(cfg.Guardrails.Allowlist) != 2 || cfg.Guardrails.Allowlist[0] != "BENCHMARK_PAYLOAD" {
		t.Fatalf("allowlist = %v", cfg.Guardrails.Allowlist)
	}
}

func TestServerTimeoutDefaultsApplied(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	if cfg.Server.ReadTimeout != 30*time.Second || cfg.Server.IdleTimeout != 90*time.Second {
		t.Fatalf("default http timeouts = %+v", cfg.Server)
	}
	if cfg.Server.IdempotencyTTL != time.Hour {
		t.Fatalf("default idempotency ttl = %s", cfg.Server.IdempotencyTTL)
	}
}
