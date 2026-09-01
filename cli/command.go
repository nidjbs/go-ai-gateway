package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Command is a saved reusable prompt with optional YAML frontmatter metadata.
// Body is the system prompt; the rest is metadata consumed by gw run and the
// scheduler.
type Command struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tools       []string `yaml:"tools"`
	Schedule    string   `yaml:"schedule"`
	Body        string   `yaml:"-"`
}

const frontmatterDelim = "---"

// parseCommand splits a command file into YAML frontmatter + markdown body.
// Files without a leading --- block are treated as plain body (backward compat
// with pre-frontmatter saved prompts).
func parseCommand(data []byte) (*Command, error) {
	cmd := &Command{}
	body := data
	if bytes.HasPrefix(data, []byte(frontmatterDelim+"\n")) {
		rest := data[len(frontmatterDelim)+1:]
		marker := []byte("\n" + frontmatterDelim + "\n")
		end := bytes.Index(rest, marker)
		if end < 0 {
			return nil, errors.New("frontmatter 缺少结束标记 ---")
		}
		if err := yaml.Unmarshal(rest[:end], cmd); err != nil {
			return nil, fmt.Errorf("解析 frontmatter: %w", err)
		}
		body = rest[end+len(marker):]
	}
	cmd.Body = strings.TrimSpace(string(body))
	return cmd, nil
}

// parseCommandOutput turns a model's distillation output into a Command,
// tolerating common format drift: surrounding code fences and a missing
// frontmatter closing marker. Falls back to treating the output as the body.
func parseCommandOutput(name, out string) (*Command, error) {
	out = strings.TrimSpace(out)
	out = strings.TrimPrefix(out, "```")
	out = strings.TrimSuffix(out, "```")
	out = strings.TrimSpace(out)
	parse := func(s string) (*Command, error) {
		cmd, err := parseCommand([]byte(s))
		if err != nil || strings.TrimSpace(cmd.Body) == "" {
			return nil, errors.New("invalid command output")
		}
		cmd.Name = name
		return cmd, nil
	}
	if cmd, err := parse(out); err == nil {
		return cmd, nil
	}
	// Repair: if the model opened frontmatter but never closed it, close it at
	// the first blank line so the rest becomes the body.
	if strings.HasPrefix(out, frontmatterDelim+"\n") {
		if idx := strings.Index(out, "\n\n"); idx > 0 {
			repaired := out[:idx] + "\n" + frontmatterDelim + out[idx:]
			if cmd, err := parse(repaired); err == nil {
				return cmd, nil
			}
		}
	}
	// Last resort: drop a stray opener and use the rest as the body.
	body := strings.TrimPrefix(out, frontmatterDelim+"\n")
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("沉淀结果为空")
	}
	return &Command{Name: name, Body: body}, nil
}

// marshal renders the full command file: frontmatter + blank line + body.
func (c *Command) marshal() ([]byte, error) {
	meta := struct {
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Tools       []string `yaml:"tools"`
		Schedule    string   `yaml:"schedule"`
	}{c.Name, c.Description, c.Tools, c.Schedule}
	fm, err := yaml.Marshal(meta)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(frontmatterDelim + "\n")
	b.Write(fm)
	b.WriteString(frontmatterDelim + "\n")
	if c.Body != "" {
		b.WriteString("\n" + c.Body + "\n")
	}
	return b.Bytes(), nil
}

// commandPath returns promptsDir()/<name>.md.
func commandPath(dir, name string) string {
	return filepath.Join(dir, name+".md")
}

// writeCommand persists a command under dir/<name>.md (0600).
func writeCommand(dir, name string, cmd *Command) error {
	cmd.Name = name
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := cmd.marshal()
	if err != nil {
		return err
	}
	return os.WriteFile(commandPath(dir, name), data, 0o600)
}

// loadCommand resolves a name the way systemPromptFor does (path/file, prompts
// dir, builtin, raw text) and returns its parsed metadata + body.
func loadCommand(arg, promptsDir string) (*Command, error) {
	if arg == "" {
		return nil, errors.New("空的命令名")
	}
	if strings.Contains(arg, string(filepath.Separator)) || strings.HasSuffix(arg, ".md") || strings.HasSuffix(arg, ".txt") {
		if data, err := os.ReadFile(arg); err == nil {
			return parseCommand(data)
		}
	}
	if promptsDir != "" {
		if data, err := os.ReadFile(commandPath(promptsDir, arg)); err == nil {
			cmd, err := parseCommand(data)
			if err != nil {
				return nil, err
			}
			if cmd.Name == "" {
				cmd.Name = arg
			}
			return cmd, nil
		}
	}
	if tpl, ok := builtinPrompts[arg]; ok {
		return &Command{Name: arg, Body: strings.Replace(tpl, "%s", "", 1)}, nil
	}
	return &Command{Name: arg, Body: strings.TrimSpace(arg)}, nil
}

// selectTools returns the agent tool specs matching the declared names;
// unknown names are ignored. Empty input means no tools are advertised.
func selectTools(names []string) []ToolSpec {
	if len(names) == 0 {
		return nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var out []ToolSpec
	for _, t := range agentTools() {
		if want[t.Function.Name] {
			out = append(out, t)
		}
	}
	return out
}
