package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAgentRules(t *testing.T) {
	state := t.TempDir()
	t.Setenv("GW_STATE_DIR", state)
	if got := loadAgentRules(); got != "" {
		t.Fatalf("absent rules = %q", got)
	}
	if err := os.WriteFile(filepath.Join(state, "agent.md"), []byte("  用中文回复\n简洁  "), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadAgentRules(); got != "用中文回复\n简洁" {
		t.Fatalf("rules = %q", got)
	}
}
