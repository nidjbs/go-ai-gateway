package usage

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistryHasSQLite(t *testing.T) {
	sink, err := Registry.Build("sqlite", map[string]any{"path": filepath.Join(t.TempDir(), "x.db")})
	if err != nil {
		t.Fatal(err)
	}
	closer, ok := sink.(io.Closer)
	if !ok {
		t.Fatalf("sqlite sink %T does not implement io.Closer", sink)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestRegistryBuildSQLiteDefaults(t *testing.T) {
	dir := t.TempDir()
	prevDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevDir) })

	sink, err := Registry.Build("sqlite", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := sink.(io.Closer); ok {
			_ = c.Close()
		}
	})
	if _, ok := sink.(io.Closer); !ok {
		t.Fatalf("sqlite sink %T does not implement io.Closer", sink)
	}

	// usage.db should have been created in the working dir.
	if _, err := os.Stat(filepath.Join(dir, "usage.db")); err != nil {
		t.Fatalf("expected usage.db in cwd: %v", err)
	}
}

func TestSQLiteSinkRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "round.db")
	sink, err := Registry.Build("sqlite", map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := sink.(io.Closer); ok {
			_ = c.Close()
		}
	})

	if err := sink.Record(context.Background(), Event{
		EventID:      "ev-round",
		RequestID:    "r1",
		Endpoint:     "chat.completions",
		Alias:        "chat",
		StatusCode:   200,
		Success:      true,
		InputTokens:  11,
		OutputTokens: 22,
		TotalTokens:  33,
		StartedAt:    time.Now().UTC(),
		CompletedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Reopen the same file via raw database/sql to assert persistence.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var (
		gotID    string
		gotInput int
		gotTotal int
	)
	if err := db.QueryRow("SELECT event_id, input_tokens, total_tokens FROM usage_events WHERE event_id = ?", "ev-round").Scan(&gotID, &gotInput, &gotTotal); err != nil {
		t.Fatal(err)
	}
	if gotID != "ev-round" || gotInput != 11 || gotTotal != 33 {
		t.Fatalf("row = %q/%d/%d", gotID, gotInput, gotTotal)
	}
}

func TestRegistryUnknownDriverStillErrors(t *testing.T) {
	if _, err := Registry.Build("redis", nil); err == nil {
		t.Fatal("expected error for unregistered driver")
	}
}
