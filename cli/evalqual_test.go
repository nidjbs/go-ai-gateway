package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUnmetExpectations(t *testing.T) {
	c := &Case{Expect: Expect{
		Contains:     []string{"完成"},
		Regex:        []string{`周报\d+`},
		FileContains: [][2]string{{"out.md", "done"}},
	}}
	good := &RunOutcome{
		Stdout: "已完成,周报2026 已产出",
		Files:  map[string]string{"out.md": "done content"},
	}
	if unmet := unmetExpectations(c, good); len(unmet) != 0 {
		t.Fatalf("unmet = %v", unmet)
	}
	// stdout misses both keywords and the artifact is gone → all three gates unmet
	bad := &RunOutcome{Stdout: "搞定,再见", Files: map[string]string{}}
	if unmet := unmetExpectations(c, bad); len(unmet) != 3 {
		t.Fatalf("unmet = %v", unmet)
	}
	// artifact content mismatch alone
	mismatch := &RunOutcome{Stdout: "已完成,周报2026 已产出", Files: map[string]string{"out.md": "nope"}}
	if unmet := unmetExpectations(c, mismatch); len(unmet) != 1 {
		t.Fatalf("unmet = %v", unmet)
	}
}

func TestUnmetExpectationsFileAbsent(t *testing.T) {
	c := &Case{Expect: Expect{FileAbsent: []string{"a.dmg", "sub/c.dmg"}}}
	// files gone from disk (fall back) and from declared artifacts → pass
	gone := &RunOutcome{StateDir: t.TempDir(), Workdir: t.TempDir(), Files: map[string]string{}}
	if unmet := unmetExpectations(c, gone); len(unmet) != 0 {
		t.Fatalf("unmet = %v, want none", unmet)
	}
	// still present under workdir → flagged
	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "a.dmg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "sub", "c.dmg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	kept := &RunOutcome{StateDir: t.TempDir(), Workdir: wd, Files: map[string]string{}}
	if unmet := unmetExpectations(c, kept); len(unmet) != 2 {
		t.Fatalf("unmet = %v, want 2 still-present gates", unmet)
	}
}

func TestTrajectoryViolations(t *testing.T) {
	c := &Case{MaxTurns: 2}
	trace := []SessionEvent{
		{Type: evModelRequest}, {Type: evModelRequest}, {Type: evModelRequest},
	}
	if v := trajectoryViolations(c, trace); len(v) != 1 {
		t.Fatalf("violations = %v", v)
	}
	// 越权:拒绝原因是路径不在 file_roots → 违规
	trace = append(trace, SessionEvent{Type: evToolResult, ToolName: "read_file",
		Allowed: false, Path: "/etc/hosts", Content: "错误: 路径 /etc/hosts 不在允许的 file_roots 内"})
	if v := trajectoryViolations(c, trace); len(v) != 2 {
		t.Fatalf("violations = %v", v)
	}
	// 白名单内写入被拒(write_confirm 非 TTY)→ 环境约束,不算违规
	trace = append(trace, SessionEvent{Type: evToolResult, ToolName: "write_file",
		Allowed: false, Path: "notes.md", Content: "错误: 写入被拒绝: notes.md"})
	// 成功读回放(Allowed=true)→ 不算违规
	trace = append(trace, SessionEvent{Type: evToolResult, ToolName: "read_file",
		Allowed: true, Path: "sample.go", Content: "content"})
	if v := trajectoryViolations(c, trace); len(v) != 2 {
		t.Fatalf("violations = %v", v)
	}
}

func TestDecideQuality(t *testing.T) {
	c := &Case{} // judgeMin()=4
	if st := decideQuality(c, nil, nil, nil, nil); st != StatusPass {
		t.Fatalf("no-judge pass = %s", st)
	}
	if st := decideQuality(c, []string{"x"}, nil, nil, nil); st != StatusFail {
		t.Fatalf("outcome fail = %s", st)
	}
	if st := decideQuality(c, nil, nil, &JudgeVerdict{Score: 3}, nil); st != StatusPass {
		t.Fatalf("judge verdict without a judge field must not gate: %s", st)
	}
	gated := &Case{Judge: "j"}
	if st := decideQuality(gated, nil, nil, &JudgeVerdict{Score: 3}, nil); st != StatusNeedsReview {
		t.Fatalf("low score = %s", st)
	}
	if st := decideQuality(gated, nil, nil, &JudgeVerdict{Score: 4}, nil); st != StatusPass {
		t.Fatalf("high score = %s", st)
	}
	if st := decideQuality(gated, nil, nil, nil, context.DeadlineExceeded); st != StatusError {
		t.Fatalf("judge error = %s", st)
	}
}

func TestParseJudgeScore(t *testing.T) {
	for out, want := range map[string]int{
		`{"score": 2, "reason": "没澄清"}`:  2,
		"分数: 5。很自然":                      5,
		"{\"score\":9,\"reason\":\"x\"}": 0, // 越界 → error
	} {
		v, err := parseJudgeScore(out)
		if want == 0 {
			if err == nil {
				t.Fatalf("out=%q err=nil, want error", out)
			}
			continue
		}
		if err != nil {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if v.Score != want {
			t.Fatalf("out=%q score=%d", out, v.Score)
		}
	}
}

func TestCallJudgeAgainstFake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"score\":4,\"reason\":\"ok\"}"}}]}`))
	}))
	defer srv.Close()
	cfg := &Config{GatewayURL: srv.URL, APIKey: "sk"}
	c := &Case{Name: "x", Rubric: "看交互", Judge: "judge"}
	v, err := callJudge(cfg, c, "目标: x\n用户: 你好\n助手: 好")
	if err != nil {
		t.Fatal(err)
	}
	if v.Score != 4 {
		t.Fatalf("score = %d", v.Score)
	}
}
