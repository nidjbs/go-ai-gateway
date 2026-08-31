package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session event types recorded by the CLI agent loop. Surface events (system,
// user, assistant, tool) are model-visible and lossless; the rest are audit.
const (
	evSessionStarted   = "session.started"
	evSystemContext    = "system.context"
	evUserMessage      = "user.message"
	evModelRequest     = "model.request"
	evAssistantMessage = "assistant.message"
	evToolCall         = "tool.call"
	evToolResult       = "tool.result"
	evAgentError       = "agent.error"
	evContextCompact   = "context.compact"
	evSessionEnded     = "session.ended"
)

// SessionEvent is one line of a session log, event-sourced: any message that
// reaches a model request must be reconstructable from it ("可见即已记录").
// Seq orders events; Role/ToolCalls/Arguments make surface events lossless;
// ShadowSeqs lets compaction and trim-replace hide older events while the log
// keeps every original; SourceSeqs records provenance.
type SessionEvent struct {
	EventID      string     `json:"event_id"`
	SessionID    string     `json:"session_id"`
	Seq          int64      `json:"seq,omitempty"`
	Type         string     `json:"type"`
	OccurredAt   time.Time  `json:"occurred_at"`
	Role         string     `json:"role,omitempty"`
	Turn         int        `json:"turn,omitempty"`
	RequestID    string     `json:"request_id,omitempty"`
	Model        string     `json:"model,omitempty"`
	ToolName     string     `json:"tool_name,omitempty"`
	ToolCallID   string     `json:"tool_call_id,omitempty"`
	Arguments    string     `json:"arguments,omitempty"`
	Path         string     `json:"path,omitempty"`
	InputTokens  int        `json:"input_tokens,omitempty"`
	OutputTokens int        `json:"output_tokens,omitempty"`
	DurationMS   int64      `json:"duration_ms,omitempty"`
	Allowed      bool       `json:"allowed,omitempty"`
	Content      string     `json:"content,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	SourceSeqs   []int64    `json:"source_seqs,omitempty"`
	ShadowSeqs   []int64    `json:"shadow_seqs,omitempty"`
	Message      string     `json:"message,omitempty"`
}

// Session is an append-only event log (mirrored in memory + JSONL file) plus
// the derived model-visible surface. It is the single source of truth for the
// conversation; replLoop and /save project messages from it.
type Session struct {
	ID     string
	file   *os.File
	events []SessionEvent
	seq    int64
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

// loadSession opens an existing session for replay + appending (--resume).
func loadSession(dir, id string) (*Session, error) {
	path := filepath.Join(dir, id+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	s := &Session{ID: id, file: f}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev SessionEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // skip corrupt lines
		}
		s.events = append(s.events, ev)
		if ev.Seq > s.seq {
			s.seq = ev.Seq
		}
	}
	// Pre-seq logs (backward compat): assign seqs in file order.
	for i := range s.events {
		if s.events[i].Seq == 0 {
			s.seq++
			s.events[i].Seq = s.seq
		}
	}
	return s, nil
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

// Emit assigns seq and appends the event to the log. Failures surface only on
// stderr; a broken log never blocks the conversation. Returns the seq.
func (s *Session) Emit(ev SessionEvent) int64 {
	s.seq++
	ev.EventID = newEventID()
	ev.SessionID = s.ID
	ev.Seq = s.seq
	ev.OccurredAt = time.Now()
	s.events = append(s.events, ev)
	data, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw: sessionlog:", err)
		return s.seq
	}
	if _, err := s.file.Write(append(data, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "gw: sessionlog:", err)
	}
	return s.seq
}

// Compact emits a compaction event hiding the given surface seqs; the originals
// stay in the log.
func (s *Session) Compact(shadowSeqs []int64) {
	s.Emit(SessionEvent{Type: evContextCompact, ShadowSeqs: shadowSeqs})
}

// EmitTrim replaces a large tool result with a trimmed one via a replace event:
// the original stays in the log (shadowed), the trimmed version joins the
// surface.
func (s *Session) EmitTrim(origSeq int64, trimmed string) {
	var orig *SessionEvent
	for i := range s.events {
		if s.events[i].Seq == origSeq {
			orig = &s.events[i]
			break
		}
	}
	if orig == nil {
		return
	}
	s.Emit(SessionEvent{
		Type: evToolResult, Role: "tool",
		ToolCallID: orig.ToolCallID, ToolName: orig.ToolName,
		Content: trimmed, SourceSeqs: []int64{origSeq}, ShadowSeqs: []int64{origSeq},
	})
}

// isSurface reports whether an event type is model-visible.
func isSurface(ev SessionEvent) bool {
	switch ev.Type {
	case evSystemContext, evUserMessage, evAssistantMessage, evToolResult:
		return true
	}
	return false
}

// roleFor infers a message role, preferring the explicit role and falling back
// to the event type (for pre-role logs).
func roleFor(ev SessionEvent) string {
	if ev.Role != "" {
		return ev.Role
	}
	switch ev.Type {
	case evSystemContext:
		return "system"
	case evUserMessage:
		return "user"
	case evAssistantMessage:
		return "assistant"
	case evToolResult:
		return "tool"
	}
	return ""
}

// surfaceEvents returns the current model-visible surface events in seq order.
func (s *Session) surfaceEvents() []SessionEvent {
	shadowed := make(map[int64]bool)
	for _, ev := range s.events {
		for _, seq := range ev.ShadowSeqs {
			shadowed[seq] = true
		}
	}
	var out []SessionEvent
	for _, ev := range s.events {
		if isSurface(ev) && !shadowed[ev.Seq] {
			out = append(out, ev)
		}
	}
	return out
}

// Messages projects the current surface into the model-visible messages.
func (s *Session) Messages() []Message {
	return project(s.surfaceEvents())
}

// FullTranscript returns the original surface messages (including compacted and
// replaced ones), used by /save to distill the complete conversation.
func (s *Session) FullTranscript() []Message {
	var originals []SessionEvent
	for _, ev := range s.events {
		if isSurface(ev) && len(ev.SourceSeqs) == 0 {
			originals = append(originals, ev)
		}
	}
	return project(originals)
}

// project maps surface events to model messages.
func project(events []SessionEvent) []Message {
	out := make([]Message, 0, len(events))
	for _, ev := range events {
		out = append(out, Message{Role: roleFor(ev), Content: ev.Content, ToolCallID: ev.ToolCallID, ToolCalls: ev.ToolCalls})
	}
	return out
}

// Close flushes and closes the session file.
func (s *Session) Close() error {
	return s.file.Close()
}
