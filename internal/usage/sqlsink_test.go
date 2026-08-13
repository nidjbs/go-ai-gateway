package usage

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "usage.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLSinkRecordAndQuery(t *testing.T) {
	db := openTestDB(t)
	sink, err := NewSQLSink(db, SQLiteSchema(), DefaultInsertSQL(DefaultTable, "?"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{
			EventID:            "ev-1",
			RequestID:          "req-1",
			APIKeyID:           "key-1",
			TeamID:             "team-1",
			Endpoint:           "chat.completions",
			Alias:              "chat",
			RequestedModel:     "chat",
			ResolvedModel:      "gpt-x",
			Provider:           "primary",
			UpstreamModel:      "gpt-x",
			ErrorType:          "",
			StreamOutcome:      "completed",
			StatusCode:         200,
			Success:            true,
			Streaming:          true,
			AttemptCount:       1,
			RetryCount:         0,
			FailoverCount:      0,
			InputTokens:        10,
			OutputTokens:       20,
			TotalTokens:        30,
			InputCostMicros:    100,
			OutputCostMicros:   200,
			TotalCostMicros:    300,
			DurationMS:         1234,
			TimeToFirstTokenMS: 250,
			StartedAt:          now,
			CompletedAt:        now.Add(2 * time.Second),
			ClientIP:           "10.0.0.1",
			UserAgent:          "curl/8",
		},
		{
			EventID:   "ev-2",
			RequestID: "req-2",
			// No api_key / team / client_ip / user_agent — should be NULL.
			Endpoint:       "embeddings",
			Alias:          "embed",
			RequestedModel: "embed",
			ResolvedModel:  "emb-model",
			Provider:       "primary",
			UpstreamModel:  "emb-model",
			StatusCode:     200,
			Success:        true,
			InputTokens:    5,
			TotalTokens:    5,
			StartedAt:      now,
			CompletedAt:    now.Add(100 * time.Millisecond),
		},
		{
			EventID:        "ev-3",
			RequestID:      "req-3",
			Endpoint:       "chat.completions",
			Alias:          "chat",
			RequestedModel: "chat",
			ResolvedModel:  "gpt-x",
			Provider:       "primary",
			UpstreamModel:  "gpt-x",
			StatusCode:     429,
			Success:        false,
			Streaming:      false,
			StartedAt:      now,
			CompletedAt:    now.Add(50 * time.Millisecond),
			ErrorType:      "rate_limit_exceeded",
		},
	}
	for _, e := range events {
		if err := sink.Record(context.Background(), e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("count = %d; want 3", count)
	}

	var sumInput int
	if err := db.QueryRow("SELECT COALESCE(SUM(input_tokens), 0) FROM usage_events").Scan(&sumInput); err != nil {
		t.Fatal(err)
	}
	if sumInput != 15 {
		t.Fatalf("sum input_tokens = %d; want 15", sumInput)
	}

	var (
		gotEventID, gotKey, gotOutcome string
		gotSuccess                     int
		gotCost                        int64
	)
	row := db.QueryRow("SELECT event_id, api_key_id, stream_outcome, success, total_cost_micros FROM usage_events WHERE event_id = ?", "ev-1")
	if err := row.Scan(&gotEventID, &gotKey, &gotOutcome, &gotSuccess, &gotCost); err != nil {
		t.Fatal(err)
	}
	if gotEventID != "ev-1" || gotKey != "key-1" || gotOutcome != "completed" || gotSuccess != 1 || gotCost != 300 {
		t.Fatalf("row = %q/%q/%q/%d/%d", gotEventID, gotKey, gotOutcome, gotSuccess, gotCost)
	}

	var nullKey sql.NullString
	if err := db.QueryRow("SELECT api_key_id FROM usage_events WHERE event_id = ?", "ev-2").Scan(&nullKey); err != nil {
		t.Fatal(err)
	}
	if nullKey.Valid {
		t.Fatalf("ev-2 api_key_id should be NULL, got %q", nullKey.String)
	}

	var status int
	if err := db.QueryRow("SELECT status FROM usage_events WHERE event_id = ?", "ev-3").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != 429 {
		t.Fatalf("status = %d; want 429", status)
	}
}

func TestSQLSinkPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	ddl := SQLiteSchema()
	insertSQL := DefaultInsertSQL(DefaultTable, "?")

	db1, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewSQLSink(db1, ddl, insertSQL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := sink.Record(context.Background(), Event{
		EventID:      "ev-persist",
		RequestID:    "r",
		Endpoint:     "chat.completions",
		StatusCode:   200,
		Success:      true,
		InputTokens:  7,
		OutputTokens: 9,
		TotalTokens:  16,
		StartedAt:    now,
		CompletedAt:  now,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	// Re-running DDL must be a no-op for an existing table.
	if _, err := db2.Exec(ddl); err != nil {
		t.Fatalf("re-init schema: %v", err)
	}
	var count int
	if err := db2.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("after reopen count = %d; want 1", count)
	}
}

func TestNewSQLSinkNilDB(t *testing.T) {
	if _, err := NewSQLSink(nil, "irrelevant", "irrelevant"); err == nil {
		t.Fatal("expected error for nil db")
	}
}

func TestNewSQLSinkBadDDL(t *testing.T) {
	db := openTestDB(t)
	_, err := NewSQLSink(db, "THIS IS NOT VALID SQL", DefaultInsertSQL(DefaultTable, "?"))
	if err == nil {
		t.Fatal("expected error for bad DDL")
	}
	if !strings.Contains(err.Error(), "init schema") {
		t.Fatalf("err = %v; want init schema wrap", err)
	}
}

func TestSQLSinkRecordAfterClose(t *testing.T) {
	db := openTestDB(t)
	sink, err := NewSQLSink(db, SQLiteSchema(), DefaultInsertSQL(DefaultTable, "?"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	err = sink.Record(context.Background(), Event{EventID: "x", RequestID: "y", Endpoint: "z"})
	if err == nil {
		t.Fatal("expected error after close")
	}
	if !errors.Is(err, io.ErrClosedPipe) && !strings.Contains(err.Error(), "closed") {
		t.Fatalf("err = %v; want closed-pipe-ish", err)
	}
}
