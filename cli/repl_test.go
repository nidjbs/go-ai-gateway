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

// newTestSession creates a session in a temp dir, cleaned up after the test.
func newTestSession(t *testing.T) *Session {
	t.Helper()
	s, err := StartSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReplLoopSaveAndExit(t *testing.T) {
	srv, reqs := replMock(t)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")
	state := t.TempDir()
	t.Setenv("GW_PROMPTS_DIR", state)
	t.Setenv("GW_SESSION_DIR", t.TempDir())

	in := strings.NewReader("hello\n/save weekly-report\n/exit\n")
	if code := replLoop(loadTestCLIConfig(t), "chat", "", "", false, in, newTestSession(t)); code != 0 {
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
	if code := replLoop(loadTestCLIConfig(t), "chat", "sys-prompt", "seed-content", false, in, newTestSession(t)); code != 0 {
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

func TestReplDefaultPrompt(t *testing.T) {
	srv, reqs := replMock(t)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")
	t.Setenv("GW_STATE_DIR", t.TempDir()) // no agent.md

	in := strings.NewReader("hi\n/exit\n")
	if code := replLoop(loadTestCLIConfig(t), "chat", "", "", false, in, newTestSession(t)); code != 0 {
		t.Fatalf("replLoop = %d", code)
	}
	if len(*reqs) == 0 {
		t.Fatal("no requests")
	}
	msgs := (*reqs)[0].Messages
	if len(msgs) == 0 || msgs[0].Role != "system" || msgs[0].Content != defaultAgentPrompt {
		t.Fatalf("default prompt not injected: %+v", msgs)
	}
}

func TestReplAgentRules(t *testing.T) {
	srv, reqs := replMock(t)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")
	state := t.TempDir()
	t.Setenv("GW_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, "agent.md"), []byte("必须用中文回复"), 0o600); err != nil {
		t.Fatal(err)
	}

	in := strings.NewReader("hi\n/exit\n")
	if code := replLoop(loadTestCLIConfig(t), "chat", "", "", false, in, newTestSession(t)); code != 0 {
		t.Fatalf("replLoop = %d", code)
	}
	if len(*reqs) == 0 {
		t.Fatal("no requests")
	}
	var lastSystem *Message
	for i := range (*reqs)[0].Messages {
		if (*reqs)[0].Messages[i].Role == "system" {
			lastSystem = &(*reqs)[0].Messages[i]
		}
	}
	if lastSystem == nil || lastSystem.Content != "必须用中文回复" {
		t.Fatalf("agent rules not injected: %+v", (*reqs)[0].Messages)
	}
}

func TestReplExitChinese(t *testing.T) {
	srv, _ := replMock(t)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")

	in := strings.NewReader("退出\n")
	if code := replLoop(loadTestCLIConfig(t), "chat", "", "", false, in, newTestSession(t)); code != 0 {
		t.Fatalf("replLoop with 退出 = %d", code)
	}
}

func TestReplResume(t *testing.T) {
	srv, reqs := replMock(t)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")
	t.Setenv("GW_STATE_DIR", t.TempDir())

	s, err := StartSession(sessionsDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Emit(SessionEvent{Type: evSystemContext, Role: "system", Content: "sys"})
	s.Emit(SessionEvent{Type: evUserMessage, Role: "user", Content: "old q"})
	s.Emit(SessionEvent{Type: evAssistantMessage, Role: "assistant", Content: "old a"})
	id := s.ID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err := in.WriteString("new q\n/exit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = in
	defer func() { os.Stdin = old }()

	if code := run([]string{"repl", "--resume", id}); code != 0 {
		t.Fatalf("run(repl --resume) = %d", code)
	}
	// The resumed context must include the replayed history + the new message.
	last := (*reqs)[len(*reqs)-1]
	want := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "old q"},
		{Role: "assistant", Content: "old a"},
		{Role: "user", Content: "new q"},
	}
	if len(last.Messages) != len(want) {
		t.Fatalf("resumed messages = %+v", last.Messages)
	}
	for i := range want {
		if !messagesEqual(last.Messages[i], want[i]) {
			t.Fatalf("messages[%d] = %+v, want %+v", i, last.Messages[i], want[i])
		}
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
