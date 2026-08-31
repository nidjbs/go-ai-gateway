package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// replReq is one captured chat request.
type replReq struct {
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// replMock serves the chat endpoint, echoing a fixed reply and recording every
// request so tests can inspect the message history.
func replMock(t *testing.T) (*httptest.Server, *[]replReq) {
	t.Helper()
	var reqs []replReq
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req replReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqs = append(reqs, req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "distilled command"}}},
		})
	})
	return httptest.NewServer(mux), &reqs
}

func loadTestCLIConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestReplLoopSaveAndExit(t *testing.T) {
	srv, reqs := replMock(t)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")
	state := t.TempDir()
	t.Setenv("GW_PROMPTS_DIR", state)
	t.Setenv("GW_SESSION_DIR", t.TempDir())

	in := strings.NewReader("hello\n/save weekly-report\n/exit\n")
	if code := replLoop(loadTestCLIConfig(t), "chat", "", "", false, in); code != 0 {
		t.Fatalf("replLoop = %d", code)
	}

	// The distill request must carry the distillation prompt + transcript.
	var saveReq *replReq
	for i := range *reqs {
		if !(*reqs)[i].Stream {
			saveReq = &(*reqs)[i]
		}
	}
	if saveReq == nil {
		t.Fatal("no non-stream (distill) request found")
	}
	if got := saveReq.Messages[0].Role; got != "system" {
		t.Fatalf("distill system role = %q", got)
	}
	if got := saveReq.Messages[1].Content; !strings.Contains(got, "[助手]") || !strings.Contains(got, "hello") {
		t.Fatalf("distill transcript = %q", got)
	}

	data, err := os.ReadFile(filepath.Join(state, "weekly-report.md"))
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := parseCommand(data)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name != "weekly-report" || cmd.Body != "distilled command" {
		t.Fatalf("saved command = %+v", cmd)
	}
}

func TestReplLoopSeedAndSystem(t *testing.T) {
	srv, reqs := replMock(t)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")
	t.Setenv("GW_SESSION_DIR", t.TempDir())

	in := strings.NewReader("hi\n/exit\n")
	if code := replLoop(loadTestCLIConfig(t), "chat", "sys-prompt", "seed-content", false, in); code != 0 {
		t.Fatalf("replLoop = %d", code)
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(*reqs))
	}
	got := (*reqs)[0].Messages
	want := []Message{{Role: "system", Content: "sys-prompt"}, {Role: "user", Content: "seed-content"}, {Role: "user", Content: "hi"}}
	if len(got) != len(want) {
		t.Fatalf("messages = %v", got)
	}
	for i := range want {
		if !messagesEqual(got[i], want[i]) {
			t.Fatalf("messages[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// messagesEqual compares Message structs (not comparable once ToolCalls exists).
func messagesEqual(a, b Message) bool {
	if a.Role != b.Role || a.Content != b.Content || a.ToolCallID != b.ToolCallID || len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i] != b.ToolCalls[i] {
			return false
		}
	}
	return true
}

func TestSaveSessionInvalid(t *testing.T) {
	cfg := &Config{GatewayURL: "http://127.0.0.1:1", APIKey: "sk-test"}
	history := []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "好"}}
	state := t.TempDir()
	t.Setenv("GW_PROMPTS_DIR", state)

	for _, name := range []string{"", "a b", "../evil", "-lead", "a/b"} {
		if err := saveSession(cfg, "chat", name, history); err == nil {
			t.Fatalf("saveSession(%q) = nil, want error", name)
		}
	}
	if err := saveSession(cfg, "chat", "no-reply", nil); err == nil {
		t.Fatal("saveSession with no assistant reply = nil, want error")
	}
	// No file must be written for the valid-name-but-empty-history case.
	if _, err := os.Stat(filepath.Join(state, "no-reply.md")); !os.IsNotExist(err) {
		t.Fatal("unexpected file written")
	}
}

func TestRunReplExit(t *testing.T) {
	srv := mockGateway(t)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")

	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err := in.WriteString("/exit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = in
	defer func() { os.Stdin = old }()
	t.Setenv("GW_SESSION_DIR", t.TempDir())

	if code := run([]string{"repl"}); code != 0 {
		t.Fatalf("run(repl) = %d", code)
	}
}
