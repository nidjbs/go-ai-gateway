package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestTrimToolResult(t *testing.T) {
	if got := trimToolResult("hello"); got != "hello" {
		t.Fatalf("short trimmed: %q", got)
	}
	medium := strings.Repeat("x", toolResultThreshold)
	if got := trimToolResult(medium); got != medium {
		t.Fatalf("at-threshold content must stay full")
	}
	big := strings.Repeat("H", toolResultHead) + strings.Repeat("M", 5000) + strings.Repeat("T", toolResultTail)
	got := trimToolResult(big)
	if len(got) >= len(big) {
		t.Fatalf("not pruned: %d", len(got))
	}
	if !strings.Contains(got, "中间省略") {
		t.Fatalf("omission marker missing")
	}
	if !strings.HasPrefix(got, strings.Repeat("H", toolResultHead)) {
		t.Fatalf("head not preserved")
	}
	if !strings.HasSuffix(got, strings.Repeat("T", toolResultTail)) {
		t.Fatalf("tail not preserved")
	}
}

// buildSession creates a session with n user+assistant pairs (2n messages).
func buildSession(t *testing.T, n int) *Session {
	t.Helper()
	s, err := StartSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for i := 0; i < n; i++ {
		s.Emit(SessionEvent{Type: evUserMessage, Role: "user", Content: fmt.Sprintf("u%d", i)})
		s.Emit(SessionEvent{Type: evAssistantMessage, Role: "assistant", Content: fmt.Sprintf("a%d", i)})
	}
	return s
}

// capacity 20, trigger 20% => highWater 16; 9 pairs (18 messages) compacts to 12.
func TestMaybeCompactSlidesOldest(t *testing.T) {
	s := buildSession(t, 9)
	maybeCompact(s, 20, 20)
	msgs := s.Messages()
	if len(msgs) != 12 {
		t.Fatalf("surface after compact = %d, want 12", len(msgs))
	}
	if msgs[0].Content != "u3" || msgs[len(msgs)-1].Content != "a8" {
		t.Fatalf("window edges = %q .. %q", msgs[0].Content, msgs[len(msgs)-1].Content)
	}
	var compacts int
	for _, ev := range s.events {
		if ev.Type == evContextCompact {
			compacts++
			if len(ev.ShadowSeqs) != 6 {
				t.Fatalf("compact shadow seqs = %v, want 6", ev.ShadowSeqs)
			}
		}
	}
	if compacts != 1 {
		t.Fatalf("compaction events = %d, want 1", compacts)
	}
}

func TestMaybeCompactNoTrigger(t *testing.T) {
	s := buildSession(t, 7) // 14 messages <= highWater 16
	maybeCompact(s, 20, 20)
	if got := len(s.Messages()); got != 14 {
		t.Fatalf("surface = %d, want 14", got)
	}
}

func TestForceCompactSlidesToLowWater(t *testing.T) {
	s := buildSession(t, 9) // 18 messages
	forceCompact(s, 20)     // force regardless of high-water
	if got := len(s.Messages()); got != 12 {
		t.Fatalf("surface after force compact = %d, want 12", got)
	}
}

func TestForceCompactPrunesRetainedToolResults(t *testing.T) {
	s, err := StartSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	big := strings.Repeat("x", toolResultThreshold+100)
	s.Emit(SessionEvent{Type: evUserMessage, Role: "user", Content: "u"})
	s.Emit(SessionEvent{Type: evToolResult, Role: "tool", ToolCallID: "c1", Content: big})
	forceCompact(s, 20)
	msgs := s.Messages()
	if len(msgs) != 2 {
		t.Fatalf("surface = %d, want 2 (nothing dropped)", len(msgs))
	}
	last := msgs[len(msgs)-1]
	if len(last.Content) >= len(big) || !strings.Contains(last.Content, "中间省略") {
		t.Fatalf("retained tool result not pruned: len=%d", len(last.Content))
	}
}

func TestMaybeCompactDisabled(t *testing.T) {
	s := buildSession(t, 30)
	maybeCompact(s, 0, 20)
	if got := len(s.Messages()); got != 60 {
		t.Fatalf("surface = %d, want 60", got)
	}
}

func TestMaybePruneToolResult(t *testing.T) {
	s, err := StartSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	big := strings.Repeat("x", toolResultThreshold+100)
	seq := s.Emit(SessionEvent{Type: evToolResult, Role: "tool", ToolCallID: "c1", Content: big})
	maybePruneToolResult(s, seq, big)
	msgs := s.Messages()
	last := msgs[len(msgs)-1]
	if last.Role != "tool" || len(last.Content) >= len(big) || !strings.Contains(last.Content, "中间省略") {
		t.Fatalf("tool result not pruned on surface: len=%d", len(last.Content))
	}
	originalKept := false
	for _, ev := range s.events {
		if ev.Type == evToolResult && ev.Content == big && len(ev.SourceSeqs) == 0 {
			originalKept = true
		}
	}
	if !originalKept {
		t.Fatal("original tool result not kept in log")
	}
}
