package main

import (
	"fmt"
	"sort"
	"strings"
)

// Status is one case's final verdict.
type Status string

const (
	StatusPass        Status = "PASS"
	StatusFail        Status = "FAIL"
	StatusDiff        Status = "DIFF"
	StatusNeedsReview Status = "NEEDS_REVIEW"
	StatusError       Status = "ERROR"
)

// JudgeVerdict is the judge's 1–5 score plus its rationale.
type JudgeVerdict struct {
	Score  int
	Reason string
}

// CaseResult is one case's row in a report.
type CaseResult struct {
	Name    string
	Status  Status
	Detail  string
	Verdict *JudgeVerdict
	Stdout  string
	Replay  string
}

func (r CaseResult) md() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s — %s\n", r.Name, r.Status)
	if r.Detail != "" {
		fmt.Fprintf(&b, "\n%s\n", r.Detail)
	}
	if r.Verdict != nil {
		fmt.Fprintf(&b, "\njudge: %d/5 — %s\n", r.Verdict.Score, r.Verdict.Reason)
	}
	if r.Replay != "" {
		fmt.Fprintf(&b, "\n```\n%s\n```\n", r.Replay)
	}
	return b.String()
}

// Report aggregates case results and renders markdown + exit code.
type Report struct {
	Results []CaseResult
}

// Markdown renders the report; quality NEEDS_REVIEW cases surface on top.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# gw eval 报告\n\n汇总: %s\n\n", r.summaryCount())
	reviewed := []CaseResult{}
	rest := []CaseResult{}
	for _, res := range r.Results {
		if res.Status == StatusNeedsReview {
			reviewed = append(reviewed, res)
		} else {
			rest = append(rest, res)
		}
	}
	if len(reviewed) > 0 {
		b.WriteString("## ⚠ 需人审（judge 低于阈值，未放行）\n\n")
		for _, res := range reviewed {
			b.WriteString(res.md())
			b.WriteString("\n")
		}
	}
	b.WriteString("## 逐 case\n\n")
	for _, res := range rest {
		b.WriteString(res.md())
		b.WriteString("\n")
	}
	return b.String()
}

// ExitCode returns 1 when any case failed review gates, else 0.
func (r Report) ExitCode() int {
	for _, res := range r.Results {
		switch res.Status {
		case StatusFail, StatusDiff, StatusNeedsReview, StatusError:
			return 1
		}
	}
	return 0
}

// condensedReplay renders a human-reviewable digest of a real multi-turn case:
// goal → user turns → agent tool actions → final reply, never the raw transcript.
func condensedReplay(c *Case, trace []SessionEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "case: %s (strategy=%s)", c.Name, c.Strategy)
	for _, ev := range trace {
		switch {
		case ev.Type == evUserMessage && ev.Role == "user":
			fmt.Fprintf(&b, "\n用户: %s", ev.Content)
		case ev.Type == evAssistantMessage && ev.Content != "":
			fmt.Fprintf(&b, "\n助手: %s", ev.Content)
		case ev.Type == evToolCall:
			fmt.Fprintf(&b, "\n→ 调用工具 %s", ev.ToolName)
			if p := toolArgPath(ev.Arguments); p != "" {
				fmt.Fprintf(&b, " %s", p)
			}
		case ev.Type == evToolResult && ev.Path != "":
			fmt.Fprintf(&b, "  ⇒ %s", ev.Path)
		}
	}
	return strings.TrimSpace(b.String())
}

// summaryCount returns a compact per-status tally, e.g. "PASS 3 · FAIL 1".
func (r Report) summaryCount() string {
	counts := map[Status]int{}
	for _, res := range r.Results {
		counts[res.Status]++
	}
	var keys []string
	for st := range counts {
		keys = append(keys, string(st))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[Status(k)]))
	}
	return strings.Join(parts, " · ")
}
