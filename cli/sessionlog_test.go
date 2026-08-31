package main

import (
	"encoding/json"
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
