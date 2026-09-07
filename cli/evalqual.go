package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// unmetExpectations lists every objective gate the run misses (outcome layer).
func unmetExpectations(c *Case, o *RunOutcome) []string {
	var unmet []string
	stdout := o.Stdout
	for _, s := range c.Expect.Contains {
		if !strings.Contains(stdout, s) {
			unmet = append(unmet, fmt.Sprintf("stdout 缺子串 %q", s))
		}
	}
	for _, re := range c.Expect.Regex {
		if !regexp.MustCompile(re).MatchString(stdout) {
			unmet = append(unmet, fmt.Sprintf("stdout 不匹配 %q", re))
		}
	}
	for _, rel := range c.Expect.FileAbsent {
		if _, ok := resolveArtifact(o.StateDir, o.Workdir, rel); ok {
			unmet = append(unmet, fmt.Sprintf("文件 %s 仍存在(应已被删除)", rel))
		}
	}
	for _, fc := range c.Expect.FileContains {
		rel, want := fc[0], fc[1]
		content, ok := o.Files[rel]
		if !ok { // fall back to disk for artifacts not declared in `artifacts:`
			content, ok = resolveArtifact(o.StateDir, o.Workdir, rel)
		}
		if !ok {
			unmet = append(unmet, fmt.Sprintf("产物 %s 不存在", rel))
			continue
		}
		if !strings.Contains(content, want) {
			unmet = append(unmet, fmt.Sprintf("产物 %s 缺子串 %q", rel, want))
		}
	}
	return unmet
}

// trajectoryViolations applies deterministic process rules to the trace.
// A denied tool only counts when the denial is an out-of-roots overreach;
// write_confirm refusals under non-interactive runs are environment, not
// misbehavior (Allowed is carried on tool.result, never on tool.call).
func trajectoryViolations(c *Case, trace []SessionEvent) []string {
	var viol []string
	reqs := 0
	for _, ev := range trace {
		switch {
		case ev.Type == evModelRequest:
			reqs++
		case ev.Type == evToolResult && !ev.Allowed && strings.Contains(ev.Content, "不在允许的 file_roots"):
			viol = append(viol, "越权工具调用被拒绝: "+ev.ToolName+" "+ev.Path)
		case ev.Type == evAgentError:
			viol = append(viol, "agent 报错: "+ev.Message)
		}
	}
	if reqs > c.maxTurns() {
		viol = append(viol, fmt.Sprintf("模型请求 %d 次,超过 max_turns=%d", reqs, c.maxTurns()))
	}
	return viol
}

// decideQuality folds objective + judge layers into the final status (spec §5.7).
func decideQuality(c *Case, unmet, viol []string, v *JudgeVerdict, jerr error) Status {
	if len(unmet) > 0 || len(viol) > 0 {
		return StatusFail
	}
	if c.Judge == "" {
		return StatusPass // 无 judge gate
	}
	if jerr != nil {
		return StatusError
	}
	if v.Score < c.judgeMin() {
		return StatusNeedsReview
	}
	return StatusPass
}

// judgePrompt builds the judge request: rubric as system + condensed replay.
func judgePrompt(c *Case, replay string) []Message {
	rubric := strings.TrimSpace(c.Rubric)
	if rubric == "" {
		rubric = "评估 agent 多轮任务的交互质量。要点:是否澄清歧义、破坏性操作前是否确认、" +
			"是否解释决策、是否有意义地推进而非空转、最终是否礼貌简洁。"
	}
	return []Message{
		{Role: "system", Content: "你是交互质量评分器。依据 rubric 对下述 case 回放打 1–5 分(整数)。" +
			"只输出一行,格式为 JSON: {\"score\": <1-5>, \"reason\": \"一句话理由\"}。\nRubric:\n" + rubric},
		{Role: "user", Content: replay},
	}
}

var scoreJSONRe = regexp.MustCompile(`"score"\s*:\s*(\d+)`)
var scoreCNRe = regexp.MustCompile(`分数\s*[:：]\s*(\d+)`)

// parseJudgeScore extracts a 1–5 integer score from a judge reply, tolerating
// JSON or a Chinese "分数:" line; anything else is an error.
func parseJudgeScore(out string) (*JudgeVerdict, error) {
	score := -1
	if m := scoreJSONRe.FindStringSubmatch(out); m != nil {
		fmt.Sscanf(m[1], "%d", &score)
	} else if m := scoreCNRe.FindStringSubmatch(out); m != nil {
		fmt.Sscanf(m[1], "%d", &score)
	}
	if score < 1 || score > 5 {
		return nil, fmt.Errorf("无法从 judge 回复解析 1–5 分: %q", strings.TrimSpace(out))
	}
	reason := strings.TrimSpace(out)
	if scoreJSONRe.MatchString(out) {
		var obj struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(out), &obj) == nil && obj.Reason != "" {
			reason = obj.Reason
		}
	}
	return &JudgeVerdict{Score: score, Reason: reason}, nil
}

// callJudge asks the judge alias to score a case's condensed replay.
func callJudge(cfg *Config, c *Case, replay string) (*JudgeVerdict, error) {
	if c.Judge == "" {
		return nil, fmt.Errorf("case %s 未声明 judge", c.Name)
	}
	out, err := NewClient(cfg).Chat(context.Background(), c.Judge, judgePrompt(c, replay))
	if err != nil {
		return nil, err
	}
	return parseJudgeScore(out)
}
