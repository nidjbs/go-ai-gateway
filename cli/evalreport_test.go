package main

import (
	"strings"
	"testing"
)

func TestReportExitCode(t *testing.T) {
	ok := Report{Results: []CaseResult{{Name: "a", Status: StatusPass}}}
	if code := ok.ExitCode(); code != 0 {
		t.Fatalf("all-pass exit = %d", code)
	}
	for _, st := range []Status{StatusFail, StatusDiff, StatusNeedsReview, StatusError} {
		r := Report{Results: []CaseResult{{Name: "a", Status: st}}}
		if code := r.ExitCode(); code == 0 {
			t.Fatalf("exit must be non-zero for %s", st)
		}
	}
}

func TestReportMarkdownNeedsReviewSection(t *testing.T) {
	rep := Report{Results: []CaseResult{
		{Name: "repl-x", Status: StatusNeedsReview,
			Verdict: &JudgeVerdict{Score: 2, Reason: "没澄清歧义"},
			Replay:  "目标: 周报\n用户: 做吧\n助手: 好"},
		{Name: "ask-a", Status: StatusPass},
	}}
	md := rep.Markdown()
	if !strings.Contains(md, "需人审") || !strings.Contains(md, "repl-x") {
		t.Fatalf("markdown lacks review section:\n%s", md)
	}
	if !strings.Contains(md, "2/5") {
		t.Fatalf("markdown lacks score:\n%s", md)
	}
	if !strings.Contains(md, "ask-a") {
		t.Fatalf("markdown lacks passing case:\n%s", md)
	}
}

func TestCondensedReplay(t *testing.T) {
	trace := []SessionEvent{
		{Type: evSystemContext, Role: "system", Content: "sys"},
		{Type: evUserMessage, Role: "user", Content: "读 a.txt 并总结"},
		{Type: evToolCall, ToolName: "read_file", Arguments: `{"path":"a.txt"}`},
		{Type: evToolResult, Role: "tool", ToolName: "read_file", Path: "a.txt", Content: "第一行"},
		{Type: evAssistantMessage, Role: "assistant", Content: "读到了"},
	}
	got := condensedReplay(&Case{Name: "x"}, trace)
	for _, want := range []string{"读 a.txt 并总结", "read_file", "读到了", "a.txt"} {
		if !strings.Contains(got, want) {
			t.Fatalf("replay missing %q:\n%s", want, got)
		}
	}
}
