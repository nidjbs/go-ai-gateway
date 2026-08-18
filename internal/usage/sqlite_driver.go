package usage

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers "sqlite" with database/sql
)

// sqliteColumnTypes is the per-column SQL type declaration used by the
// bundled SQLite schema. Order must match DefaultColumns.
var sqliteColumnTypes = []string{
	"TEXT", "TEXT", "TEXT", "TEXT",
	"TEXT", "TEXT", "TEXT", "TEXT",
	"TEXT", "TEXT", "TEXT", "TEXT",
	"INTEGER", "INTEGER", "INTEGER", "INTEGER",
	"INTEGER", "INTEGER", "INTEGER", "INTEGER",
	"INTEGER", "INTEGER", "INTEGER", "INTEGER",
	"INTEGER", "INTEGER", "TIMESTAMP", "TIMESTAMP",
	"TEXT", "TEXT",
}

// sqliteIndexDefs is the (name, column) pair list for indexes created
// alongside the bundled table.
var sqliteIndexDefs = []struct{ name, column string }{
	{"idx_usage_events_started_at", "started_at"},
	{"idx_usage_events_api_key_id", "api_key_id"},
	{"idx_usage_events_team_id", "team_id"},
}

const sqliteDDL = "" // populated by initSchema below; kept as a const ref point for tests.

// init registers the SQLite driver. The factory opens a *sql.DB via modernc.org/sqlite
// (no CGo), runs the canonical schema, and returns a *SQLSink that implements io.Closer.
func init() {
	Registry.Register("sqlite", func(opts map[string]any) (Sink, error) {
		path := stringOption(opts, "path", "usage.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return nil, fmt.Errorf("open sqlite %s: %w", path, err)
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
		}
		// SQLite serialises writes internally; a single connection avoids
		// "database is locked" surprises for default single-process use. A
		// higher-concurrency deployment can set options.max_connections to
		// override.
		if _, ok := opts["max_connections"]; ok {
			db.SetMaxOpenConns(intOption(opts, "max_connections", 1))
		} else {
			db.SetMaxOpenConns(1)
		}
		return NewSQLSink(db, SQLiteSchema(), DefaultInsertSQL(DefaultTable, "?"))
	})
}

// SQLiteSchema returns the bundled CREATE TABLE / INDEX block for the default
// usage_events table. Exported so other drivers (and tests) can reuse the
// column declarations. The DDL is built once and memoized.
var sqliteSchemaDDL = buildSQLiteSchema()

func buildSQLiteSchema() string {
	cols := DefaultColumns()
	if len(cols) != len(sqliteColumnTypes) {
		panic("usage: DefaultColumns / sqliteColumnTypes length mismatch")
	}
	lines := make([]string, len(cols))
	for i, name := range cols {
		lines[i] = name + " " + sqliteColumnTypes[i]
	}
	ddl := "CREATE TABLE IF NOT EXISTS " + DefaultTable + " (\n  " +
		joinLines(lines) + "\n);"
	for _, idx := range sqliteIndexDefs {
		ddl += "\nCREATE INDEX IF NOT EXISTS " + idx.name + " ON " + DefaultTable + " (" + idx.column + ");"
	}
	return ddl
}

func joinLines(parts []string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += ",\n  " + p
	}
	return out
}

// SQLiteSchema returns the bundled CREATE TABLE / INDEX DDL.
func SQLiteSchema() string { return sqliteSchemaDDL }

// stringOption pulls a string from map[string]any with a default fallback.
func stringOption(opts map[string]any, key, def string) string {
	if opts == nil {
		return def
	}
	if v, ok := opts[key].(string); ok && v != "" {
		return v
	}
	return def
}

// intOption pulls an int from map[string]any (accepting int/int64/float64) with a default.
func intOption(opts map[string]any, key string, def int) int {
	if opts == nil {
		return def
	}
	switch v := opts[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}
