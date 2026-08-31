package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionLogJSONL(t *testing.T) {
	dir := t.TempDir()
	s, err := StartSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Fatal("empty session id")
	}
	s.Emit(SessionEvent{Type: evUserMessage, Content: "hi"})
	s.Emit(SessionEvent{Type: evSessionEnded})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	var ev SessionEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != evUserMessage || ev.SessionID != s.ID || ev.EventID == "" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.OccurredAt.IsZero() {
		t.Fatalf("occurred_at is zero: %+v", ev)
	}
}

func TestSessionMessagesProjection(t *testing.T) {
	s, err := StartSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Emit(SessionEvent{Type: evSessionStarted})
	s.Emit(SessionEvent{Type: evSystemContext, Role: "system", Content: "sys"})
	s.Emit(SessionEvent{Type: evUserMessage, Role: "user", Content: "hi"})
	s.Emit(SessionEvent{Type: evModelRequest, InputTokens: 10})
	s.Emit(SessionEvent{Type: evAssistantMessage, Role: "assistant", Content: "你好"})

	want := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "你好"},
	}
	msgs := s.Messages()
	if len(msgs) != len(want) {
		t.Fatalf("messages = %d, want %d", len(msgs), len(want))
	}
	for i := range want {
		if !messagesEqual(msgs[i], want[i]) {
			t.Fatalf("messages[%d] = %+v, want %+v", i, msgs[i], want[i])
		}
	}
}

func TestSessionCompactAndFullTranscript(t *testing.T) {
	s, err := StartSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 4; i++ {
		s.Emit(SessionEvent{Type: evUserMessage, Role: "user", Content: fmt.Sprintf("u%d", i)})
	}
	s.Compact([]int64{1, 2})
	msgs := s.Messages()
	if len(msgs) != 2 || msgs[0].Content != "u2" || msgs[1].Content != "u3" {
		t.Fatalf("surface after compact = %+v", msgs)
	}
	full := s.FullTranscript()
	if len(full) != 4 {
		t.Fatalf("full transcript = %d, want 4", len(full))
	}
}

func TestSessionTrimReplace(t *testing.T) {
	s, err := StartSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	big := strings.Repeat("x", 5000)
	s.Emit(SessionEvent{Type: evUserMessage, Role: "user", Content: "u"})
	s.Emit(SessionEvent{Type: evToolResult, Role: "tool", ToolCallID: "c1", Content: big}) // seq 2
	s.EmitTrim(2, "trimmed")
	msgs := s.Messages()
	if last := msgs[len(msgs)-1]; last.Content != "trimmed" {
		t.Fatalf("surface tool = %q, want trimmed", last.Content)
	}
	found := false
	for _, m := range s.FullTranscript() {
		if m.Role == "tool" && m.Content == big {
			found = true
		}
	}
	if !found {
		t.Fatal("original tool result not in full transcript")
	}
}

func TestLoadSessionReplay(t *testing.T) {
	dir := t.TempDir()
	s, err := StartSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Emit(SessionEvent{Type: evUserMessage, Role: "user", Content: "a"})
	s.Emit(SessionEvent{Type: evAssistantMessage, Role: "assistant", Content: "b"})
	id := s.ID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	msgs := loaded.Messages()
	if len(msgs) != 2 || msgs[1].Content != "b" {
		t.Fatalf("replayed messages = %+v", msgs)
	}
}

func TestSessionIDUnique(t *testing.T) {
	if sessionID() == sessionID() {
		t.Fatal("session ids must differ")
	}
}

func TestNewEventIDShape(t *testing.T) {
	if id := newEventID(); len(id) != 32 {
		t.Fatalf("event id len = %d, want 32", len(id))
	}
}
