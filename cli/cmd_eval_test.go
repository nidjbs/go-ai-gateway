package main

import (
	"strings"
	"testing"
)

// evalSnapshot must spawn the real CLI binary (cliBinPath), so tests build one
// and override the package-level path for the duration of the test.
func TestEvalSnapshotGoldenLifecycle(t *testing.T) {
	bin := buildCLIBin(t)
	old := cliBinPath
	cliBinPath = func() string { return bin }
	defer func() { cliBinPath = old }()

	root := t.TempDir()
	c := &Case{
		Name: "ask-hello", Strategy: StrategyMock,
		Command: "ask", Input: "你好",
		Stub: []StubStep{{Text: "你好，我是 gw，可以帮你处理重复工作。"}},
	}
	// 首次: golden 缺失 → FAIL 并提示 --record
	rep := evalSnapshot([]*Case{c}, root, false, false)
	if got := rep.Results[0].Status; got != StatusFail {
		t.Fatalf("missing golden status = %s", got)
	}
	if !strings.Contains(rep.Results[0].Detail, "--record") {
		t.Fatalf("detail should hint --record: %q", rep.Results[0].Detail)
	}
	// --record: 生成 golden → PASS
	rep = evalSnapshot([]*Case{c}, root, true, false)
	if got := rep.Results[0].Status; got != StatusPass {
		t.Fatalf("record status = %s\n%s", got, rep.Results[0].Detail)
	}
	// 再跑: 命中 golden → PASS
	rep = evalSnapshot([]*Case{c}, root, false, false)
	if got := rep.Results[0].Status; got != StatusPass {
		t.Fatalf("re-run status = %s", got)
	}
	// 有意变更(行为漂移) → DIFF
	c.Stub = []StubStep{{Text: "行为漂移了"}}
	rep = evalSnapshot([]*Case{c}, root, false, false)
	if got := rep.Results[0].Status; got != StatusDiff {
		t.Fatalf("drift status = %s\n%s", got, rep.Results[0].Detail)
	}
}
