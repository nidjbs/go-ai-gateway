package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScheduleSetAndUnset(t *testing.T) {
	t.Setenv("GW_STATE_DIR", t.TempDir())
	if err := writeCommand(promptsDir(), "report", &Command{Name: "report", Body: "正文"}); err != nil {
		t.Fatal(err)
	}
	if code := scheduleSet([]string{"report", "0 9 * * 1"}); code != 0 {
		t.Fatalf("set = %d", code)
	}
	got, _ := parseCommand(readCommandFile(t, "report"))
	if got.Schedule != "0 9 * * 1" {
		t.Fatalf("schedule = %q", got.Schedule)
	}
	if code := scheduleUnset([]string{"report"}); code != 0 {
		t.Fatalf("unset = %d", code)
	}
	got, _ = parseCommand(readCommandFile(t, "report"))
	if got.Schedule != "" {
		t.Fatalf("schedule after unset = %q", got.Schedule)
	}
}

func TestScheduleSetInvalid(t *testing.T) {
	t.Setenv("GW_STATE_DIR", t.TempDir())
	if err := writeCommand(promptsDir(), "report", &Command{Name: "report", Body: "正文"}); err != nil {
		t.Fatal(err)
	}
	if code := scheduleSet([]string{"report", "not-a-cron"}); code == 0 {
		t.Fatal("invalid cron must fail")
	}
}

func TestScheduleSetMissingCommand(t *testing.T) {
	t.Setenv("GW_STATE_DIR", t.TempDir())
	if code := scheduleSet([]string{"nope", "0 9 * * 1"}); code == 0 {
		t.Fatal("missing command must fail")
	}
}

func TestNextRun(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	next, err := nextRun("@every 24h", now)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("next = %v", next)
	}
	if _, err := nextRun("bogus", now); err == nil {
		t.Fatal("bad spec must fail")
	}
}

func TestScheduledCommandsLists(t *testing.T) {
	t.Setenv("GW_STATE_DIR", t.TempDir())
	if err := writeCommand(promptsDir(), "a", &Command{Name: "a", Body: "x", Schedule: "@daily"}); err != nil {
		t.Fatal(err)
	}
	if err := writeCommand(promptsDir(), "b", &Command{Name: "b", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	items, err := scheduledCommands(promptsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Command.Name != "a" {
		t.Fatalf("items = %+v", items)
	}
}

func TestScheduleRunDue(t *testing.T) {
	srv, _ := agentMock(t, filepath.Join(t.TempDir(), "x"))
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")
	t.Setenv("GW_STATE_DIR", t.TempDir())
	if err := writeCommand(promptsDir(), "report", &Command{Name: "report", Body: "正文", Schedule: "@every 1m"}); err != nil {
		t.Fatal(err)
	}
	// No last-run recorded → due immediately.
	out := captureStdout(t, func() int { return scheduleRun(nil) })
	if !strings.Contains(out, "ran 1") {
		t.Fatalf("stdout = %q", out)
	}
	// Second run must be a no-op (last run just recorded).
	out = captureStdout(t, func() int { return scheduleRun(nil) })
	if !strings.Contains(out, "ran 0") {
		t.Fatalf("second run stdout = %q", out)
	}
}

func readCommandFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(commandPath(promptsDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
