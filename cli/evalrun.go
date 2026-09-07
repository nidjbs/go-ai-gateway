package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// cliBinPath resolves the CLI binary used for spawned cases; tests override it
// with a freshly built binary.
var cliBinPath = func() string {
	b, err := os.Executable()
	if err != nil {
		return ""
	}
	return b
}

// RunOpts controls one spawned case execution.
type RunOpts struct {
	Bin      string        // empty → cliBinPath()
	Gateway  string        // non-empty: child GW_GATEWAY_URL (mock determinism / test fake)
	Config   string        // non-empty: child GW_CONFIG (quality: user's real CLI config)
	StateDir string        // empty → fresh temp dir
	Workdir  string        // empty → fresh temp dir under the case (fixture c.Workdir copied here)
	Stdin    string        // piped to child stdin (repl scripts)
	Timeout  time.Duration // 0 → 120s
}

// RunOutcome captures what one spawned case produced.
type RunOutcome struct {
	ExitCode int
	Stdout   string
	Stderr   string
	StateDir string // materialized state dir (sessions/prompts live under it)
	Workdir  string
	Files    map[string]string // declared artifacts: rel path → raw content
	Trace    []SessionEvent    // parsed session log(s), seq order
}

// runCase spawns the CLI child for c with isolated dirs, pipes script stdin and
// a timeout, then collects stdout/stderr/exit, artifacts and session trace.
func runCase(c *Case, o RunOpts) (*RunOutcome, error) {
	bin := o.Bin
	if bin == "" {
		bin = cliBinPath()
	}
	if bin == "" {
		return nil, fmt.Errorf("无法定位 CLI 二进制")
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	base := o.StateDir
	if base == "" {
		d, err := os.MkdirTemp("", "gw-eval-*")
		if err != nil {
			return nil, err
		}
		base = d // kept until results are read; the process or OS cleans it up
	}
	stateDir := filepath.Join(base, "state")
	workdir := filepath.Join(base, "work")
	for _, d := range []string{stateDir, filepath.Join(stateDir, "sessions"), filepath.Join(stateDir, "prompts")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if o.Workdir != "" {
		workdir = o.Workdir
	}
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		return nil, err
	}
	if c.Workdir != "" { // mount fixture dir content into the temp workdir
		if err := copyDir(c.Workdir, workdir); err != nil {
			return nil, err
		}
	}
	if c.WriteConfirm != "" { // destructive case: child config overlaid, real config untouched
		cfgPath := filepath.Join(base, "child-config.yaml")
		if err := writeEvalConfig(cfgPath, o.Config, c.WriteConfirm); err != nil {
			return nil, err
		}
		o.Config = cfgPath
	}
	argv, err := c.argv()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = workdir
	cmd.Env = childEnv(o, stateDir, workdir)
	if c.Script != "" {
		cmd.Stdin = strings.NewReader(c.Script)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	exit := 0
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("case 超时(%s)", timeout)
		}
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			return nil, err
		}
	}
	out := &RunOutcome{
		ExitCode: exit,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		StateDir: stateDir,
		Workdir:  workdir,
		Files:    map[string]string{},
		Trace:    loadTraceSilent(filepath.Join(stateDir, "sessions")),
	}
	for _, rel := range c.Artifacts {
		if content, ok := resolveArtifact(stateDir, workdir, rel); ok {
			out.Files[rel] = content
		}
	}
	return out, nil
}

// childEnv overlays a spawned child's env: temp state/prompts/session isolation,
// file roots pinned to the workdir; plus strategy-specific gateway/config.
func childEnv(o RunOpts, stateDir, workdir string) []string {
	env := os.Environ()
	set := func(k, v string) {
		env = append(env, k+"="+v)
	}
	set("GW_STATE_DIR", stateDir)
	set("GW_PROMPTS_DIR", filepath.Join(stateDir, "prompts"))
	set("GW_SESSION_DIR", filepath.Join(stateDir, "sessions"))
	set("GW_FILE_ROOTS", workdir)
	if o.Gateway != "" {
		set("GW_GATEWAY_URL", o.Gateway)
		set("GW_API_KEY", "sk-eval")
	}
	if o.Config != "" {
		set("GW_CONFIG", o.Config)
	}
	return env
}

// resolveArtifact finds rel under stateDir first, then workdir.
func resolveArtifact(stateDir, workdir, rel string) (string, bool) {
	for _, root := range []string{stateDir, workdir} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err == nil {
			return string(data), true
		}
	}
	return "", false
}

// loadTraceSilent parses all session jsonl files in dir (sorted), best-effort.
func loadTraceSilent(dir string) []SessionEvent {
	evs, _ := loadTrace(dir)
	return evs
}

// loadTrace parses every <id>.jsonl in dir into seq-ordered events.
func loadTrace(dir string) ([]SessionEvent, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []SessionEvent
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var ev SessionEvent
			if json.Unmarshal([]byte(line), &ev) == nil {
				out = append(out, ev)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// normalizeFunc returns a replacer applying pairs longest-key-first.
func normalizeFunc(pairs [][2]string) func(string) string {
	sorted := make([][2]string, len(pairs))
	copy(sorted, pairs)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i][0]) > len(sorted[j][0]) })
	return func(s string) string {
		for _, p := range sorted {
			s = strings.ReplaceAll(s, p[0], p[1])
		}
		return s
	}
}

var sessionIDRe = regexp.MustCompile(`[0-9]{8}T[0-9]{6}-[0-9a-f]{8}`)

// copyDir copies src's entries (files recursively) into dst.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}
