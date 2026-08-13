package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SQLSink is a Sink backed by any *sql.DB. The driver package (sqlite_driver
// or a third-party init() that registers "postgres"/"mysql"/...) supplies the
// DDL string and INSERT statement, so schema is owned by the driver and not
// hardcoded here. This keeps the backend swappable without touching core code.
//
// Record executes the INSERT inside the caller's context. The handler
// already wraps the call with context.WithTimeout(..., time.Second) so a
// stuck database cannot stall the request goroutine indefinitely; on failure
// the sink returns an error and the handler logs a warning — the user request
// itself is unaffected.
type SQLSink struct {
	db        *sql.DB
	insertSQL string
}

// NewSQLSink runs the schema DDL (idempotent CREATE TABLE / INDEX IF NOT
// EXISTS) and stores the INSERT statement for later use. On schema failure
// the supplied db is closed.
func NewSQLSink(db *sql.DB, ddl, insertSQL string) (*SQLSink, error) {
	if db == nil {
		return nil, fmt.Errorf("sql sink: db is nil")
	}
	if _, err := db.Exec(ddl); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sql sink: init schema: %w", err)
	}
	return &SQLSink{db: db, insertSQL: insertSQL}, nil
}

// Record maps Event onto the INSERT column list. Column order is fixed and
// documented in DefaultInsertSQL — keep both in sync.
func (s *SQLSink) Record(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, s.insertSQL,
		e.EventID,
		e.RequestID,
		nullString(e.APIKeyID),
		nullString(e.TeamID),
		nullString(e.Endpoint),
		nullString(e.Alias),
		nullString(e.RequestedModel),
		nullString(e.ResolvedModel),
		nullString(e.Provider),
		nullString(e.UpstreamModel),
		nullString(e.ErrorType),
		nullString(e.StreamOutcome),
		e.StatusCode,
		boolInt(e.Success),
		boolInt(e.Streaming),
		e.AttemptCount,
		e.RetryCount,
		e.FailoverCount,
		e.InputTokens,
		e.OutputTokens,
		e.TotalTokens,
		e.InputCostMicros,
		e.OutputCostMicros,
		e.TotalCostMicros,
		e.DurationMS,
		e.TimeToFirstTokenMS,
		e.StartedAt.UTC(),
		e.CompletedAt.UTC(),
		nullString(e.ClientIP),
		nullString(e.UserAgent),
	)
	return err
}

// Close releases the underlying *sql.DB. server.Run shutdown detects SQLSink
// via io.Closer type assertion and calls this.
func (s *SQLSink) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DefaultInsertSQL builds the INSERT statement for the SQLSink column list.
// The placeholders string must match the target dialect: "?" for SQLite/MySQL,
// "$1, $2, ..." for Postgres (already-joined by the driver). The column list
// is canonical and matches DefaultColumns.
func DefaultInsertSQL(table string, placeholders string) string {
	cols := DefaultColumns()
	ph := make([]string, len(cols))
	for i := range ph {
		ph[i] = placeholders
	}
	return fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		table,
		strings.Join(cols, ", "),
		strings.Join(ph, ", "),
	)
}

// DefaultColumns returns the canonical column list, in the exact order Record
// supplies values. Third-party drivers that want schema alignment should
// reuse this list.
func DefaultColumns() []string {
	return []string{
		"event_id", "request_id", "api_key_id", "team_id",
		"endpoint", "alias", "requested_model", "resolved_model",
		"provider", "upstream_model", "error_type", "stream_outcome",
		"status", "success", "streaming", "attempts",
		"retries", "failovers", "input_tokens", "output_tokens",
		"total_tokens", "input_cost_micros", "output_cost_micros", "total_cost_micros",
		"duration_ms", "ttft_ms", "started_at", "completed_at",
		"client_ip", "user_agent",
	}
}

// DefaultTable is the table name used by the bundled SQLite driver and a
// sensible default for third-party drivers.
const DefaultTable = "usage_events"

func nullString(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
