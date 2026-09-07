package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var buildOnce sync.Once
var builtBin string

// buildCLIBin compiles the real CLI once per test process.
func buildCLIBin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		bin := filepath.Join(os.TempDir(), "gw-eval-test-bin")
		out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
		if err != nil {
			t.Fatalf("build cli: %v\n%s", err, out)
		}
		builtBin = bin
	})
	return builtBin
}

func TestRunCaseAskMock(t *testing.T) {
	bin := buildCLIBin(t)
	f := newEvalFake([]StubStep{{Text: "你好，我是 gw。"}}, "")
	defer f.Close()
	c := &Case{Name: "ask-hello", Command: "ask", Input: "hi"}
	res, err := runCase(c, RunOpts{Bin: bin, Gateway: f.URL(), Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "你好，我是 gw。" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if res.StateDir == "" || res.Workdir == "" {
		t.Fatal("state/workdir must be materialized")
	}
	if len(res.Files) != 0 {
		t.Fatalf("files = %v", res.Files)
	}
}

func TestRunCaseReplSaveCancelWritesNothing(t *testing.T) {
	bin := buildCLIBin(t)
	f := newEvalFake([]StubStep{
		{Text: "好的，开始。"},
		{Text: "should not be consumed because cancelled"},
	}, "")
	defer f.Close()
	c := &Case{
		Name: "repl-save", Command: "repl",
		Script:    "你好\n/save weekly-report 每周一早汇总上周销售\nn\n退出\n",
		Artifacts: []string{"prompts/weekly-report.md"},
	}
	res, err := runCase(c, RunOpts{Bin: bin, Gateway: f.URL(), Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if _, ok := res.Files["prompts/weekly-report.md"]; ok {
		t.Fatal("cancelled save must not produce the artifact")
	}
	if len(res.Trace) == 0 {
		t.Fatal("trace must be captured from the session log")
	}
}

func TestNormalizePairs(t *testing.T) {
	norm := normalizeFunc([][2]string{{"/a/state", "<state>"}, {"/a/state/sess", "<session>"}})
	got := norm("跑 /a/state/sess/1 与 /a/state/x")
	want := "跑 <session>/1 与 <state>/x"
	if got != want {
		t.Fatalf("normalize = %q, want %q", got, want)
	}
}
