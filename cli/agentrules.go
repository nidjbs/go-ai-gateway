package main

import (
	"os"
	"path/filepath"
	"strings"
)

// agentRulesPath returns <state>/agent.md (~/.config/gw/agent.md by default,
// following GW_STATE_DIR).
func agentRulesPath() string {
	return filepath.Join(gwStateDir(), "agent.md")
}

// loadAgentRules returns the user's global agent.md conventions (fixed style,
// rules) to inject into agent sessions, or "" when the file is absent.
func loadAgentRules() string {
	data, err := os.ReadFile(agentRulesPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
