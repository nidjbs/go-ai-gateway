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
	long := strings.Repeat("x", maxToolResultBytes+100)
	got := trimToolResult(long)
	if len(got) >= len(long) || !strings.Contains(got, "已裁剪") {
		t.Fatalf("long not trimmed: len=%d", len(got))
	}
}

// window 20, trigger 20% => highWater 16; the 17th add compresses to 12 (60%).
func TestContextCompressorTrigger(t *testing.T) {
	c := newContextCompressor("", 20, 20)
	for i := 0; i < 16; i++ {
		c.add(Message{Role: "user", Content: fmt.Sprintf("m%d", i)})
	}
	if got := len(c.requestMessages()); got != 16 {
		t.Fatalf("after 16 adds window = %d, want 16", got)
	}
	c.add(Message{Role: "user", Content: "m16"})
	msgs := c.requestMessages()
	if len(msgs) != 12 {
		t.Fatalf("after trigger window = %d, want 12", len(msgs))
	}
	if msgs[len(msgs)-1].Content != "m16" {
		t.Fatalf("last message = %q, want m16", msgs[len(msgs)-1].Content)
	}
}

// A large tool result stays full until the trigger, then gets trimmed.
func TestContextCompressorTrimsToolResults(t *testing.T) {
	c := newContextCompressor("", 20, 20)
	for i := 0; i < 16; i++ {
		c.add(Message{Role: "user", Content: "u"})
	}
	big := strings.Repeat("x", maxToolResultBytes+100)
	c.add(Message{Role: "tool", Content: big}) // 17th -> trigger
	msgs := c.requestMessages()
	last := msgs[len(msgs)-1]
	if last.Role != "tool" {
		t.Fatalf("last role = %q", last.Role)
	}
	if len(last.Content) >= len(big) || !strings.Contains(last.Content, "已裁剪") {
		t.Fatalf("tool result not trimmed: len=%d", len(last.Content))
	}
}

func TestContextCompressorDisabled(t *testing.T) {
	c := newContextCompressor("", 0, 20)
	for i := 0; i < 50; i++ {
		c.add(Message{Role: "user", Content: "u"})
	}
	if got := len(c.requestMessages()); got != 50 {
		t.Fatalf("disabled window = %d, want 50", got)
	}
}

func TestContextCompressorKeepsSystem(t *testing.T) {
	c := newContextCompressor("sys-prompt", 3, 20) // highWater 2, lowWater 1
	for i := 0; i < 5; i++ {
		c.add(Message{Role: "user", Content: "u"})
	}
	msgs := c.requestMessages()
	if msgs[0].Role != "system" || msgs[0].Content != "sys-prompt" {
		t.Fatalf("system prompt lost: %+v", msgs[0])
	}
}
