package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestNewProductionFieldsParse(t *testing.T) {
	data := []byte("listen: 127.0.0.1:8080\nhealthz: 127.0.0.1:8081\nops_token_env: OPS_TOKEN\nauth:\n  mode: api-key\nteams:\n  - id: team-a\n    api_keys:\n      - id: key-a\n        key: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n        limits:\n          max_requests_per_day: 5\n          max_tokens_per_request: 100\nproviders:\n  p:\n    base_url: http://localhost:1\naliases:\n  chat:\n    provider: p\n    model: m\nserver:\n  stream_idle_timeout: 2m\n  stream_max_duration: 15m\n  read_timeout: 45s\n  idle_timeout: 120s\n  idempotency_enabled: true\n  idempotency_ttl: 2h\nguardrails:\n  enabled: true\n  mode: block\n  allowlist:\n    - BENCHMARK_PAYLOAD\n    - RED_TEAM\n")

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

func TestForceUsageTriStateParses(t *testing.T) {
	parse := func(t *testing.T, line string) *bool {
		data := []byte("listen: 127.0.0.1:8080\nhealthz: 127.0.0.1:8081\nproviders:\n  p:\n    base_url: http://x\n" + line + "\naliases:\n  chat:\n    provider: p\n    model: m\n")
		var cfg Config
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Providers["p"].ForceUsage
	}

	if got := parse(t, "    force_usage: true"); got == nil || !*got {
		t.Fatalf("force_usage: true -> %v", got)
	}
	if got := parse(t, "    force_usage: false"); got == nil || *got {
		t.Fatalf("force_usage: false -> %v", got)
	}
	if got := parse(t, ""); got != nil {
		t.Fatalf("unset force_usage -> %v (want nil)", got)
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
