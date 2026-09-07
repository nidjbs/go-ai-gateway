package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadCaseFileDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ask-hello.yaml")
	content := "name: ask-hello\nstrategy: mock\ncommand: ask\ninput: 你好\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadCaseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "ask-hello" || c.Strategy != StrategyMock {
		t.Fatalf("case = %+v", c)
	}
	if got := c.alias(); got != "chat" {
		t.Fatalf("alias() = %q", got)
	}
	if got := c.judgeMin(); got != 4 {
		t.Fatalf("judgeMin() = %d", got)
	}
	if got := c.maxTurns(); got != 12 {
		t.Fatalf("maxTurns() = %d", got)
	}
	if got := c.timeout(); got != 120*time.Second {
		t.Fatalf("timeout() = %v", got)
	}
}

func TestCaseArgv(t *testing.T) {
	ask := &Case{Command: "ask", Input: "hi"}
	argv, err := ask.argv()
	if err != nil || len(argv) != 3 || argv[0] != "ask" || argv[2] != "hi" {
		t.Fatalf("ask argv = %v, %v", argv, err)
	}
	run := &Case{Command: "run", Args: []string{"./demo.md", "跑一下"}}
	if argv, err := run.argv(); err != nil || argv[0] != "run" || argv[1] != "./demo.md" {
		t.Fatalf("run argv = %v, %v", argv, err)
	}
	repl := &Case{Command: "repl"}
	if argv, err := repl.argv(); err != nil || argv[0] != "repl" || argv[1] != "-m" {
		t.Fatalf("repl argv = %v, %v", argv, err)
	}
	bad := &Case{Command: "run"}
	if _, err := bad.argv(); err == nil {
		t.Fatal("run without a saved name must error")
	}
}

func TestLoadCaseInvalid(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"bad-strategy.yaml", "bad-command.yaml", "empty.yaml", "bad-name.yaml",
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(""), 0o600)
		if _, err := loadCaseFile(p); err == nil {
			t.Fatalf("loadCaseFile(%s) = nil, want error", name)
		}
	}
	// invalid write_confirm rejected; valid one accepted
	bad := filepath.Join(dir, "bad-wc.yaml")
	os.WriteFile(bad, []byte("name: x\nstrategy: mock\ncommand: ask\ninput: hi\nwrite_confirm: sometimes\n"), 0o600)
	if _, err := loadCaseFile(bad); err == nil {
		t.Fatal("invalid write_confirm must error")
	}
	good := filepath.Join(dir, "good-wc.yaml")
	os.WriteFile(good, []byte("name: x\nstrategy: mock\ncommand: ask\ninput: hi\nwrite_confirm: never\n"), 0o600)
	if c, err := loadCaseFile(good); err != nil || c.WriteConfirm != "never" {
		t.Fatalf("valid write_confirm rejected: %v, %v", c, err)
	}
}
