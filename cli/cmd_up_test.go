package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFirstAliasSorted(t *testing.T) {
	aliases := map[string]yaml.Node{"b": {}, "a": {}, "c": {}}
	if got := firstAlias(aliases); got != "a" {
		t.Fatalf("firstAlias = %q, want a", got)
	}
	if got := firstAlias(map[string]yaml.Node{}); got != "chat" {
		t.Fatalf("empty firstAlias = %q, want chat fallback", got)
	}
}

func TestInferAPIKey(t *testing.T) {
	var cfg gatewayConfig
	cfg.Auth.Mode = "api-key"
	cfg.Teams = []struct {
		APIKeys []struct {
			Key string `yaml:"key"`
		} `yaml:"api_keys"`
	}{{APIKeys: []struct {
		Key string `yaml:"key"`
	}{{Key: "sk-first"}, {Key: "sk-second"}}}}
	if got := inferAPIKey(&cfg); got != "sk-first" {
		t.Fatalf("inferAPIKey = %q, want sk-first", got)
	}

	cfg = gatewayConfig{}
	cfg.Auth.Mode = "static"
	cfg.Auth.TokenEnv = "GW_TEST_TOKEN"
	t.Setenv("GW_TEST_TOKEN", "tok-value")
	if got := inferAPIKey(&cfg); got != "tok-value" {
		t.Fatalf("static infer = %q", got)
	}
}

func TestRandomToken(t *testing.T) {
	if len(randomToken()) != 16 {
		t.Fatal("random token must be 16 hex chars")
	}
}

func TestResolveGatewayConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(p, []byte("listen: 127.0.0.1:1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveGatewayConfig(p); got != p {
		t.Fatalf("arg must win: %q", got)
	}
	t.Setenv("GW_GATEWAY_CONFIG", p)
	if got, _ := resolveGatewayConfig(""); got != p {
		t.Fatalf("env must win: %q", got)
	}
}

func TestReadGatewayConfigInjectsNone(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gw.yaml")
	body := "listen: 127.0.0.1:8080\nauth:\n  mode: none\nproviders:\n  p:\n    type: openai\n    base_url: http://x/v1\naliases:\n  chat:\n    provider: p\n    model: m\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, raw, err := readGatewayConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin != nil {
		t.Fatal("admin must be nil for injection")
	}
	if !strings.Contains(raw, "aliases:") {
		t.Fatal("raw must preserve original config")
	}
}
