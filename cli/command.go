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
