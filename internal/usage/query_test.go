package usage

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestSink(t *testing.T) *SQLSink {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	s, err := NewSQLSink(db, SQLiteSchema(), DefaultInsertSQL(DefaultTable, "?"))
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustRecord(t *testing.T, s *SQLSink, e Event) {
	t.Helper()
	if err := s.Record(context.Background(), e); err != nil {
		t.Fatalf("record: %v", err)
	}
}

func TestSQLSinkSummary(t *testing.T) {
	s := newTestSink(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	mustRecord(t, s, Event{APIKeyID: "k1", TeamID: "t1", Alias: "chat", Success: true, InputTokens: 10, OutputTokens: 20, TotalTokens: 30, TotalCostMicros: 5, DurationMS: 100, StartedAt: now.Add(-time.Hour), CompletedAt: now.Add(-time.Hour + time.Second)})
	mustRecord(t, s, Event{APIKeyID: "k1", TeamID: "t1", Alias: "chat", Success: true, InputTokens: 1, OutputTokens: 2, TotalTokens: 3, TotalCostMicros: 7, DurationMS: 50, StartedAt: now.Add(-2 * time.Hour), CompletedAt: now})
	mustRecord(t, s, Event{APIKeyID: "k2", TeamID: "t2", Alias: "embed", Success: false, InputTokens: 100, OutputTokens: 0, TotalTokens: 100, TotalCostMicros: 99, DurationMS: 10, StartedAt: now.Add(-3 * time.Hour), CompletedAt: now})

	sum, err := s.Summary(context.Background(), QueryFilter{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Requests != 3 || sum.Successes != 2 || sum.Failures != 1 {
		t.Fatalf("counts wrong: %+v", sum)
	}
	if sum.InputTokens != 111 || sum.OutputTokens != 22 || sum.TotalTokens != 133 {
		t.Fatalf("tokens wrong: %+v", sum)
	}
	if sum.CostMicros != 111 || sum.DurationMS != 160 {
		t.Fatalf("cost/duration wrong: %+v", sum)
	}

	// Filter by team.
	teamSum, err := s.Summary(context.Background(), QueryFilter{TeamID: "t1", From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("team summary: %v", err)
	}
	if teamSum.Requests != 2 || teamSum.TotalTokens != 33 {
		t.Fatalf("team filter wrong: %+v", teamSum)
	}

	// Filter by api key + alias.
	keySum, err := s.Summary(context.Background(), QueryFilter{APIKeyID: "k2", Alias: "embed", From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("key summary: %v", err)
	}
	if keySum.Requests != 1 || keySum.Successes != 0 || keySum.Failures != 1 {
		t.Fatalf("key filter wrong: %+v", keySum)
	}
}

func TestSQLSinkSeries(t *testing.T) {
	s := newTestSink(t)
	now := time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC)
	// Two events in the same day bucket, one in a different day.
	mustRecord(t, s, Event{APIKeyID: "k1", Success: true, TotalTokens: 10, StartedAt: now.Add(-2 * time.Hour), CompletedAt: now})
	mustRecord(t, s, Event{APIKeyID: "k1", Success: true, TotalTokens: 20, StartedAt: now.Add(-3 * time.Hour), CompletedAt: now})
	mustRecord(t, s, Event{APIKeyID: "k1", Success: false, TotalTokens: 5, StartedAt: now.Add(-26 * time.Hour), CompletedAt: now})

	buckets, err := s.Series(context.Background(), QueryFilter{From: now.Add(-48 * time.Hour), To: now.Add(time.Hour)}, 24*time.Hour)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 day buckets, got %d: %+v", len(buckets), buckets)
	}
	// buckets are ordered by start; day-2 event is older.
	if buckets[0].Start.Day() == now.Day() {
		t.Fatalf("bucket[0] should be the older day, got %v", buckets[0].Start)
	}
	if buckets[0].Requests != 1 || buckets[0].TotalTokens != 5 {
		t.Fatalf("older day wrong: %+v", buckets[0])
	}
	if buckets[1].Requests != 2 || buckets[1].TotalTokens != 30 || buckets[1].Successes != 2 {
		t.Fatalf("same day wrong: %+v", buckets[1])
	}
}

func TestSQLSinkSeriesHour(t *testing.T) {
	s := newTestSink(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	mustRecord(t, s, Event{APIKeyID: "k1", Success: true, TotalTokens: 7, StartedAt: now.Add(-30 * time.Minute), CompletedAt: now})
	mustRecord(t, s, Event{APIKeyID: "k1", Success: true, TotalTokens: 3, StartedAt: now.Add(-90 * time.Minute), CompletedAt: now})
	buckets, err := s.Series(context.Background(), QueryFilter{From: now.Add(-4 * time.Hour), To: now.Add(time.Hour)}, time.Hour)
	if err != nil {
		t.Fatalf("series hour: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 hour buckets, got %d", len(buckets))
	}
	if buckets[0].TotalTokens != 3 || buckets[1].TotalTokens != 7 {
		t.Fatalf("hour buckets wrong: %+v", buckets)
	}
}
