package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// qualityGateway is a deterministic fake that doubles as both the agent gateway
// and the judge gateway: the first chat request (agent) answers with a
// keyword-bearing sentence; later requests (judge) return a low-score JSON.
func qualityGateway(t *testing.T) *httptest.Server {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"周报 2026 已生成"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"score\":2,\"reason\":\"未先确认就删文件\"}"}}]}`))
	}))
	return srv
}

func TestEvalQualityWithJudgeGate(t *testing.T) {
	bin := buildCLIBin(t)
	old := cliBinPath
	cliBinPath = func() string { return bin }
	defer func() { cliBinPath = old }()

	gw := qualityGateway(t)
	defer gw.Close()
	cfg := &Config{GatewayURL: gw.URL, APIKey: "sk"}

	gate := &Case{
		Name: "judge-low", Strategy: StrategyReal,
		Command: "ask", Input: "生成周报",
		Expect: Expect{Contains: []string{"周报"}},
		Judge:  "judge", JudgeMin: intPtr(4),
		Rubric: "交互质量",
	}
	rep := evalQuality([]*Case{gate}, cfg, gw.URL)
	res := rep.Results[0]
	if res.Status != StatusNeedsReview {
		t.Fatalf("status = %s (want NEEDS_REVIEW)\n%s", res.Status, res.Detail)
	}
	if res.Verdict == nil || res.Verdict.Score != 2 {
		t.Fatalf("verdict = %+v", res.Verdict)
	}
	if !strings.Contains(rep.Markdown(), "需人审") {
		t.Fatal("markdown 缺需人审区")
	}
	if rep.ExitCode() == 0 {
		t.Fatal("NEEDS_REVIEW must exit non-zero")
	}
}

func TestEvalQualityObjectiveFail(t *testing.T) {
	bin := buildCLIBin(t)
	old := cliBinPath
	cliBinPath = func() string { return bin }
	defer func() { cliBinPath = old }()

	f := newEvalFake([]StubStep{{Text: "我没有这个能力。"}}, "")
	defer f.Close()
	cfg := &Config{GatewayURL: f.URL(), APIKey: "sk"}
	c := &Case{Name: "obj-fail", Strategy: StrategyReal, Command: "ask", Input: "hi",
		Expect: Expect{Contains: []string{"周报"}}}
	rep := evalQuality([]*Case{c}, cfg, f.URL())
	if got := rep.Results[0].Status; got != StatusFail {
		t.Fatalf("status = %s (want FAIL)", got)
	}
}
