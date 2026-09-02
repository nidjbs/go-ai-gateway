package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func writeClipboardHistory(t *testing.T, entries ...clipboardEntry) {
	t.Helper()
	t.Setenv("GW_STATE_DIR", t.TempDir())
	f, err := os.OpenFile(clipboardPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range entries {
		data, _ := json.Marshal(e)
		f.Write(append(data, '\n'))
	}
}

func TestFindClipboard(t *testing.T) {
	writeClipboardHistory(t,
		clipboardEntry{Time: "2026-09-01T10:00:00Z", Text: "hello world 你好"},
		clipboardEntry{Time: "2026-09-01T11:00:00Z", Text: "another clipboard line"},
	)
	out, err := findClipboard("你好", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello world 你好") {
		t.Fatalf("full match missing: %q", out)
	}
	out, _ = findClipboard("nonexistent", 3)
	if !strings.Contains(out, "没有匹配") {
		t.Fatalf("no-match message missing: %q", out)
	}
	t.Setenv("GW_STATE_DIR", t.TempDir())
	out, _ = findClipboard("x", 3)
	if !strings.Contains(out, "剪贴板历史为空") {
		t.Fatalf("empty-history message missing: %q", out)
	}
}

// leet-normalized fuzzy recall: "password" surfaces the full "P@ssw0rd123".
func TestFindClipboardLeet(t *testing.T) {
	writeClipboardHistory(t,
		clipboardEntry{Time: "2026-09-01T10:00:00Z", Text: "the weather is nice"},
		clipboardEntry{Time: "2026-09-01T11:00:00Z", Text: "P@ssw0rd123"},
	)
	out, _ := findClipboard("password", 3)
	if !strings.Contains(out, "P@ssw0rd123") || strings.Contains(out, "weather") {
		t.Fatalf("leet recall wrong: %q", out)
	}
}

func TestFindClipboardBoundsLimit(t *testing.T) {
	entries := make([]clipboardEntry, 5)
	for i := range entries {
		entries[i] = clipboardEntry{Time: fmt.Sprintf("t%d", i), Text: fmt.Sprintf("match %d", i)}
	}
	writeClipboardHistory(t, entries...)
	out, _ := findClipboard("match", 2)
	if strings.Count(out, "=== ") != 2 {
		t.Fatalf("limit not applied: %q", out)
	}
}

func TestTrimClipboardHistory(t *testing.T) {
	t.Setenv("GW_STATE_DIR", t.TempDir())
	f, err := os.OpenFile(clipboardPath(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < clipboardMaxEntries+20; i++ {
		e := clipboardEntry{Time: "t", Text: fmt.Sprintf("line %d", i)}
		data, _ := json.Marshal(e)
		f.Write(append(data, '\n'))
	}
	f.Close()
	trimClipboardHistory()
	data, err := os.ReadFile(clipboardPath())
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n"); len(lines) != clipboardMaxEntries {
		t.Fatalf("lines = %d, want %d", len(lines), clipboardMaxEntries)
	}
}

// recallClipboard: local model (mock) returns the entry number; the full text
// is read from the file and returned.
func TestRecallClipboard(t *testing.T) {
	writeClipboardHistory(t,
		clipboardEntry{Time: "t1", Text: "P@ssw0rd123"},
		clipboardEntry{Time: "t2", Text: "the weather is nice"},
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "1"}}},
		})
	}))
	defer srv.Close()
	cfg := &Config{GatewayURL: srv.URL, APIKey: "sk", ClipboardLocalAlias: "local"}
	text, err := recallClipboard(cfg, "password")
	if err != nil {
		t.Fatal(err)
	}
	if text != "P@ssw0rd123" {
		t.Fatalf("recall = %q", text)
	}
}

func TestRecallClipboardNoAlias(t *testing.T) {
	writeClipboardHistory(t, clipboardEntry{Time: "t", Text: "x"})
	if _, err := recallClipboard(&Config{}, "q"); err == nil {
		t.Fatal("missing clipboard_local_alias must error")
	}
}
