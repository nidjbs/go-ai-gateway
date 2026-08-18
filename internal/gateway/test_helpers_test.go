package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// testLogger returns a slog.Logger that discards all output so tests don't
// pollute the test runner. We still honour the level so future debugging
// can dial it up via t.Setenv if needed.
func testLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testContext returns a context that is canceled when the test ends. Use
// this for any blocking call into Redis or the HTTP layer so the test
// suite can't wedge on a hung dependency.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// guard true time helper used to keep gofmt happy when the file imports
// time but doesn't otherwise use it. Cheap; kept in one place so future
// test files don't need to re-import.
var _ = time.Now