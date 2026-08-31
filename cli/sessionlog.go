package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Session event types recorded by the CLI agent loop. The flat snake_case
// schema mirrors internal/events.Event so future context engineering can map
// both sources uniformly.
const (
	evSessionStarted   = "session.started"
	evUserMessage      = "user.message"
	evModelRequest     = "model.request"
	evAssistantMessage = "assistant.message"
	evToolCall         = "tool.call"
	evToolResult       = "tool.result"
	evAgentError       = "agent.error"
	evSessionEnded     = "session.ended"
)

// SessionEvent is one line of a session log. RequestID correlates with
// gateway-side events (the CLI forwards it as X-Request-Id).
type SessionEvent struct {
	EventID      string    `json:"event_id"`
	SessionID    string    `json:"session_id"`
	Type         string    `json:"type"`
	OccurredAt   time.Time `json:"occurred_at"`
	Turn         int       `json:"turn,omitempty"`
	RequestID    string    `json:"request_id,omitempty"`
	Model        string    `json:"model,omitempty"`
	ToolName     string    `json:"tool_name,omitempty"`
	ToolCallID   string    `json:"tool_call_id,omitempty"`
	Path         string    `json:"path,omitempty"`
	InputTokens  int       `json:"input_tokens,omitempty"`
	OutputTokens int       `json:"output_tokens,omitempty"`
	DurationMS   int64     `json:"duration_ms,omitempty"`
	Allowed      bool      `json:"allowed,omitempty"`
	Content      string    `json:"content,omitempty"`
	Message      string    `json:"message,omitempty"`
}

// Session appends structured events to one JSONL file under the sessions dir.
type Session struct {
	ID   string
	file *os.File
}

// StartSession opens <dir>/<id>.jsonl for appending (0600).
func StartSession(dir string) (*Session, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id := sessionID()
	f, err := os.OpenFile(filepath.Join(dir, id+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Session{ID: id, file: f}, nil
}

// sessionID is a timestamp plus a random suffix, unique per session.
func sessionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().Format("20060102T150405")
	}
	return time.Now().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// newEventID returns a 32-hex-char unique id (same shape as events.NewEventID).
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// Emit writes one event as a JSON line. Failures surface only on stderr; a
// broken log never blocks the conversation.
func (s *Session) Emit(ev SessionEvent) {
	ev.EventID = newEventID()
	ev.SessionID = s.ID
	data, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw: sessionlog:", err)
		return
	}
	if _, err := s.file.Write(append(data, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "gw: sessionlog:", err)
	}
}

// Close flushes and closes the session file.
func (s *Session) Close() error {
	return s.file.Close()
}
