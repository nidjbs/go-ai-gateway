package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Strategy picks how a case is evaluated: mock = deterministic golden snapshot,
// real = driven against a real gateway/LLM and judged on outcome+trace+quality.
type Strategy string

const (
	StrategyMock Strategy = "mock"
	StrategyReal Strategy = "real"
)

// Expect are objective gates for quality cases: substrings/regexes in stdout and
// substrings inside resolved artifact files. FileAbsent asserts a file was really
// deleted (deterministic check for destructive scenarios).
type Expect struct {
	Contains     []string    `yaml:"contains"`
	Regex        []string    `yaml:"regex"`
	FileContains [][2]string `yaml:"file_contains"`
	FileAbsent   []string    `yaml:"file_absent"`
}

// StubTool is one tool call the deterministic responder returns.
type StubTool struct {
	Name      string         `yaml:"name"`
	Arguments map[string]any `yaml:"args"`
}

// StubStep is one deterministic reply, consumed per model request.
type StubStep struct {
	Text     string    `yaml:"text"`
	ToolCall *StubTool `yaml:"tool_call"`
}

// NormalizeRule is a case-specific path/text → placeholder replacement.
type NormalizeRule struct {
	Pattern     string `yaml:"pattern"`
	Placeholder string `yaml:"placeholder"`
}

// Case is one eval scenario file (schema mirrors spec §5.1).
type Case struct {
	Name      string          `yaml:"name"`
	Strategy  Strategy        `yaml:"strategy"`
	Command   string          `yaml:"command"`
	Alias     string          `yaml:"alias"`
	Input     string          `yaml:"input"`
	Args      []string        `yaml:"args"`
	Script    string          `yaml:"script"`
	Workdir   string          `yaml:"workdir"`
	Stub      []StubStep      `yaml:"stub"`
	Fallback  string          `yaml:"fallback"`
	Expect    Expect          `yaml:"expect"`
	Artifacts []string        `yaml:"artifacts"`
	Rubric    string          `yaml:"rubric"`
	Judge     string          `yaml:"judge"`
	JudgeMin  *int            `yaml:"judge_min"`
	MaxTurns  int             `yaml:"max_turns"`
	Timeout   string          `yaml:"timeout"`
	Normalize []NormalizeRule `yaml:"normalize"`
	// WriteConfirm lets a destructive scenario actually mutate inside its sandbox:
	// the child's write_confirm is overlaid to this value on a derived config file,
	// never touching the user's real config. auto/always in a non-TTY eval would
	// otherwise deny every delete/write.
	WriteConfirm string `yaml:"write_confirm"`
}

var caseNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (c *Case) alias() string { return firstNonEmpty(c.Alias, "chat") }
func (c *Case) judgeMin() int {
	if c.JudgeMin != nil {
		return *c.JudgeMin
	}
	return 4
}
func (c *Case) maxTurns() int {
	if c.MaxTurns > 0 {
		return c.MaxTurns
	}
	return 12
}
func (c *Case) timeout() time.Duration {
	if d, err := time.ParseDuration(c.Timeout); err == nil {
		return d
	}
	return 120 * time.Second
}

// argv returns the spawned CLI subprocess argv for this case (never "eval").
func (c *Case) argv() ([]string, error) {
	switch c.Command {
	case "ask", "trans", "summarize", "explain":
		text := strings.TrimSpace(c.Input)
		if text == "" {
			text = strings.Join(c.Args, " ")
		}
		if text == "" {
			return nil, fmt.Errorf("%s: 缺少输入", c.Command)
		}
		return []string{c.Command, "--no-stream", text}, nil
	case "models":
		return []string{"models"}, nil
	case "run":
		if len(c.Args) == 0 {
			return nil, fmt.Errorf("run: 需要已保存命令名(args[0])")
		}
		return append([]string{"run"}, c.Args...), nil
	case "repl":
		return []string{"repl", "-m", c.alias()}, nil
	default:
		return nil, fmt.Errorf("不支持的 command %q", c.Command)
	}
}

// loadCaseFile parses and validates one case YAML.
func loadCaseFile(path string) (*Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Case
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !caseNameRe.MatchString(c.Name) {
		return nil, fmt.Errorf("%s: 缺合法 name", path)
	}
	if c.Strategy != StrategyMock && c.Strategy != StrategyReal {
		return nil, fmt.Errorf("%s: strategy 必须是 mock 或 real", path)
	}
	if _, err := c.argv(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	switch c.WriteConfirm {
	case "", "auto", "always", "never":
	default:
		return nil, fmt.Errorf("%s: 无效 write_confirm %q (可选: auto/always/never)", path, c.WriteConfirm)
	}
	if c.Workdir != "" && !filepath.IsAbs(c.Workdir) { // fixture dir relative to the case file
		c.Workdir = filepath.Join(filepath.Dir(path), c.Workdir)
	}
	return &c, nil
}

// loadCases reads every *.yaml/*.yml in dir, sorted by filename.
func loadCases(dir string) ([]*Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Case
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext != ".yaml" && ext != ".yml" {
			continue
		}
		c, err := loadCaseFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: 没有找到任何 case", dir)
	}
	return out, nil
}
