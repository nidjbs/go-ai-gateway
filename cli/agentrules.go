package main

import (
	"os"
	"path/filepath"
	"strings"
)

// defaultAgentPrompt positions the assistant when no --system is given: a
// personal assistant for quick, repetitive tasks.
const defaultAgentPrompt = `你是用户的个人 AI 助手，定位是快速帮用户处理简单、重复的工作。回答简洁，直接给出结果。`

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
