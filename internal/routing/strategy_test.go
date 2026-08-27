package routing

import (
	"testing"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

func testCandidates() []Candidate {
	return []Candidate{
		{Name: "a", Model: "m-a", Priority: 0, Weight: 3},
		{Name: "b", Model: "m-b", Priority: 1, Weight: 1},
		{Name: "c", Model: "m-c", Priority: 2, Weight: 0},
	}
}

func TestParseStrategyDefaultsToFallback(t *testing.T) {
	for in, want := range map[string]Strategy{
		"":              StrategyFallback,
		"fallback":      StrategyFallback,
		"loadbalance":   StrategyLoadBalance,
		"least_latency": StrategyLeastLatency,
		"bogus":         StrategyFallback,
	} {
		if got := ParseStrategy(in); got != want {
			t.Errorf("ParseStrategy(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStrategyForReadsAliasConfig(t *testing.T) {
	cfg := &config.Config{Aliases: map[string]config.Alias{
		"chat":    {Strategy: "least_latency"},
		"embed":   {},
		"default": {Strategy: ""},
	}}
	if got := StrategyFor(cfg, "chat"); got != StrategyLeastLatency {
		t.Errorf("chat = %q, want least_latency", got)
	}
	if got := StrategyFor(cfg, "embed"); got != StrategyFallback {
		t.Errorf("embed = %q, want fallback", got)
	}
	if got := StrategyFor(cfg, "default"); got != StrategyFallback {
		t.Errorf("default = %q, want fallback", got)
	}
}

func TestOrderFallbackKeepsPriority(t *testing.T) {
	got := Order(StrategyFallback, testCandidates(), NewLatencyTracker(), func(int) int { return 0 })
	if len(got) != 3 || got[0].Name != "a" || got[1].Name != "b" || got[2].Name != "c" {
		t.Fatalf("fallback order changed: %+v", names(got))
	}
}

func TestOrderLoadBalanceWeightedPrimary(t *testing.T) {
	// randFn picking the upper end of the range must land on the second
	// configured candidate, proving weights bias the draw.
	got := Order(StrategyLoadBalance, testCandidates(), NewLatencyTracker(), func(n int) int {
		return n - 1
	})
	if got[0].Name != "b" {
		t.Fatalf("high draw selected %q, want b", got[0].Name)
	}
	// The primary is removed from the fallbacks, which stay in priority order.
	if got[1].Name != "a" || got[2].Name != "c" {
		t.Fatalf("fallbacks = %q, want [a c]", names(got[1:]))
	}
}

func TestOrderLoadBalanceZeroDrawPicksFirst(t *testing.T) {
	got := Order(StrategyLoadBalance, testCandidates(), NewLatencyTracker(), func(int) int { return 0 })
	if got[0].Name != "a" {
		t.Fatalf("zero draw selected %q, want a", got[0].Name)
	}
}

func TestOrderLoadBalanceAllZeroWeightsKeepsOrder(t *testing.T) {
	cands := []Candidate{
		{Name: "a", Model: "m-a", Priority: 0},
		{Name: "b", Model: "m-b", Priority: 1},
	}
	got := Order(StrategyLoadBalance, cands, NewLatencyTracker(), func(int) int { return 0 })
	if got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("unweighted loadbalance changed order: %q", names(got))
	}
}

func TestOrderLoadBalancePreservesSet(t *testing.T) {
	for draw := 0; draw < 8; draw++ {
		got := Order(StrategyLoadBalance, testCandidates(), NewLatencyTracker(), func(int) int { return draw })
		if len(got) != 3 || !sameNames(got, testCandidates()) {
			t.Fatalf("draw %d dropped/reordered set: %q", draw, names(got))
		}
	}
}

func TestOrderLeastLatencyFastestFirst(t *testing.T) {
	tracker := NewLatencyTracker()
	tracker.Record(Candidate{Name: "a", Model: "m-a"}, 100*time.Millisecond)
	tracker.Record(Candidate{Name: "c", Model: "m-c"}, 20*time.Millisecond)
	// b unobserved: it must be probed first so it can enter the ranking.
	got := Order(StrategyLeastLatency, testCandidates(), tracker, func(int) int { return 0 })
	want := []string{"b", "c", "a"}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("order[%d] = %q, want %q (full %q)", i, got[i].Name, name, names(got))
		}
	}
}

func TestOrderLeastLatencyTiesBreakByPriority(t *testing.T) {
	tracker := NewLatencyTracker()
	tracker.Record(Candidate{Name: "b", Model: "m-b"}, 50*time.Millisecond)
	tracker.Record(Candidate{Name: "a", Model: "m-a"}, 50*time.Millisecond)
	// c is unobserved and probes first; the two observed ties order by priority.
	got := Order(StrategyLeastLatency, testCandidates(), tracker, func(int) int { return 0 })
	if got[1].Name != "a" || got[2].Name != "b" {
		t.Fatalf("tie order = %q, want [c a b]", names(got))
	}
}

func TestOrderLeastLatencyNilTrackerKeepsPriority(t *testing.T) {
	got := Order(StrategyLeastLatency, testCandidates(), nil, func(int) int { return 0 })
	if got[0].Name != "a" || got[2].Name != "c" {
		t.Fatalf("nil tracker order = %q, want priority order", names(got))
	}
}

func TestLatencyTrackerEWMAConverges(t *testing.T) {
	tracker := NewLatencyTracker()
	c := Candidate{Name: "a", Model: "m-a"}
	for range 10 {
		tracker.Record(c, 100*time.Millisecond)
	}
	d, ok := tracker.EWMA(c)
	if !ok {
		t.Fatal("expected observation")
	}
	if d < 99*time.Millisecond || d > 101*time.Millisecond {
		t.Fatalf("EWMA = %v, want ~100ms", d)
	}
	if _, ok := tracker.EWMA(Candidate{Name: "b", Model: "m-b"}); ok {
		t.Fatal("unobserved candidate reported observed")
	}
}

func TestLatencyTrackerClampsNegative(t *testing.T) {
	tracker := NewLatencyTracker()
	c := Candidate{Name: "a", Model: "m-a"}
	tracker.Record(c, -5*time.Millisecond)
	d, ok := tracker.EWMA(c)
	if !ok || d < 0 {
		t.Fatalf("negative duration stored: %v ok=%v", d, ok)
	}
}

func names(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func sameNames(a, b []Candidate) bool {
	got, want := names(a), names(b)
	if len(got) != len(want) {
		return false
	}
	set := map[string]int{}
	for _, n := range got {
		set[n]++
	}
	for _, n := range want {
		if set[n] == 0 {
			return false
		}
		set[n]--
	}
	return true
}
