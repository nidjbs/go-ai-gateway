package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func newCapturingSink(t *testing.T) (*AuditSink, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return &AuditSink{logger: logger, level: slog.LevelInfo}, buf
}

func decodeLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(strings.Split(buf.String(), "\n")[0])
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode audit line %q: %v", line, err)
	}
	return got
}

func TestAuditSinkWritesAllFields(t *testing.T) {
	sink, buf := newCapturingSink(t)
	event := Event{
		EventID:        "ev-1",
		RequestID:      "req-1",
		Endpoint:       "chat.completions",
		Alias:          "chat",
		RequestedModel: "chat",
		ResolvedModel:  "qwen2.5:7b",
		Provider:       "ollama",
		UpstreamModel:  "qwen2.5:7b",
		APIKeyID:       "key-1",
		TeamID:         "team-1",
		StatusCode:     200,
		Success:        true,
		Streaming:      false,
		InputTokens:    10,
		OutputTokens:   20,
		TotalTokens:    30,
		DurationMS:     42,
		ClientIP:       "10.0.0.1",
		UserAgent:      "curl/8",
	}
	if err := sink.Record(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	got := decodeLine(t, buf)
	if got["msg"] != "audit" {
		t.Fatalf("msg = %v; want audit", got["msg"])
	}
	if got["event_id"] != "ev-1" {
		t.Fatalf("event_id = %v", got["event_id"])
	}
	if got["status"].(float64) != 200 {
		t.Fatalf("status = %v", got["status"])
	}
	if got["input_tokens"].(float64) != 10 {
		t.Fatalf("input_tokens = %v", got["input_tokens"])
	}
	if got["client_ip"] != "10.0.0.1" {
		t.Fatalf("client_ip = %v", got["client_ip"])
	}
	if got["level"] != "INFO" {
		t.Fatalf("level = %v; want INFO for success", got["level"])
	}
}

func TestAuditSinkFailedEventsEscalateLevel(t *testing.T) {
	sink, buf := newCapturingSink(t)
	sink = sink.WithLevel(slog.LevelInfo)
	event := Event{EventID: "ev-2", Success: false, StatusCode: 502, ErrorType: "upstream_error"}
	if err := sink.Record(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	got := decodeLine(t, buf)
	if got["level"] != "WARN" {
		t.Fatalf("failed event level = %v; want WARN", got["level"])
	}
	if got["error_type"] != "upstream_error" {
		t.Fatalf("error_type = %v", got["error_type"])
	}
}

func TestAuditSinkRespectsHigherConfiguredLevel(t *testing.T) {
	sink, buf := newCapturingSink(t)
	sink = sink.WithLevel(slog.LevelError)
	event := Event{EventID: "ev-3", Success: true, StatusCode: 200}
	if err := sink.Record(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("expected one line at Error level")
	}
}

func TestNewEventIDUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		id := NewEventID()
		if len(id) != 32 {
			t.Fatalf("id length = %d; want 32 hex chars", len(id))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q at iteration %d", id, i)
		}
		seen[id] = struct{}{}
	}
}
