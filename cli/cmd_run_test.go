package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func() int) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := fn()
	w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	return string(data)
}

func TestRunCommandWithTools(t *testing.T) {
	t.Setenv("GW_SESSION_DIR", t.TempDir())
	root := t.TempDir()
	filePath := filepath.Join(root, "f.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := agentMock(t, filePath)
	defer srv.Close()

	cfg := &Config{GatewayURL: srv.URL, APIKey: "sk-test", DefaultAlias: "chat", FileRoots: []string{root}}
	cmd := &Command{Name: "readme", Body: "读取并总结。", Tools: []string{"read_file"}}
	if out := captureStdout(t, func() int { return runCommand(cfg, "chat", cmd, "read f.txt") }); out != "done\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRunDefaultPrompt(t *testing.T) {
	t.Setenv("GW_SESSION_DIR", t.TempDir())
	t.Setenv("GW_STATE_DIR", t.TempDir()) // no agent.md
	var got []Message
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = req.Messages
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &Config{GatewayURL: srv.URL, APIKey: "sk-test", DefaultAlias: "chat"}
	cmd := &Command{Name: "empty"} // no body
	captureStdout(t, func() int { return runCommand(cfg, "chat", cmd, "hi") })
	if len(got) != 2 || got[0].Role != "system" || got[0].Content != defaultAgentPrompt || got[1].Role != "user" {
		t.Fatalf("default prompt not injected: %+v", got)
	}
}

func TestRunAgentRules(t *testing.T) {
	t.Setenv("GW_SESSION_DIR", t.TempDir())
	state := t.TempDir()
	t.Setenv("GW_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, "agent.md"), []byte("必须用中文"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []Message
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = req.Messages
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &Config{GatewayURL: srv.URL, APIKey: "sk-test", DefaultAlias: "chat"}
	cmd := &Command{Name: "plain", Body: "你是助手。"}
	captureStdout(t, func() int { return runCommand(cfg, "chat", cmd, "hi") })
	if len(got) != 3 {
		t.Fatalf("messages = %d, want 3 (rules + body + user)", len(got))
	}
	if got[0].Role != "system" || got[0].Content != "必须用中文" {
		t.Fatalf("first message = %+v, want agent rules", got[0])
	}
	if got[1].Role != "system" || got[1].Content != "你是助手。" {
		t.Fatalf("second message = %+v, want command body", got[1])
	}
	if got[2].Role != "user" {
		t.Fatalf("third = %+v", got[2])
	}
}

func TestRunCommandNoToolsSingleTurn(t *testing.T) {
	t.Setenv("GW_SESSION_DIR", t.TempDir())
	hadTools := false
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, hadTools = req["tools"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "hi"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &Config{GatewayURL: srv.URL, APIKey: "sk-test", DefaultAlias: "chat"}
	cmd := &Command{Name: "plain", Body: "你是简单助手。"}
	if out := captureStdout(t, func() int { return runCommand(cfg, "chat", cmd, "hi") }); out != "hi\n" {
		t.Fatalf("stdout = %q", out)
	}
	if hadTools {
		t.Fatal("command without tools must not advertise tools")
	}
}

func TestRunCommandNoInputAutonomous(t *testing.T) {
	t.Setenv("GW_SESSION_DIR", t.TempDir())
	msgLen := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		msgLen = len(req.Messages)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &Config{GatewayURL: srv.URL, APIKey: "sk-test", DefaultAlias: "chat"}
	cmd := &Command{Name: "auto", Body: "自主执行。"}
	if out := captureStdout(t, func() int { return runCommand(cfg, "chat", cmd, "") }); out != "ok\n" {
		t.Fatalf("stdout = %q", out)
	}
	if msgLen != 1 {
		t.Fatalf("messages = %d, want 1 (system only)", msgLen)
	}
}

func TestCmdRunSavedCommand(t *testing.T) {
	srv, _ := agentMock(t, filepath.Join(t.TempDir(), "x"))
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")
	t.Setenv("GW_STATE_DIR", t.TempDir())
	if err := writeCommand(promptsDir(), "report", &Command{Name: "report", Body: "正文", Tools: []string{"read_file"}}); err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() int { return run([]string{"run", "report", "hi"}) }); !strings.Contains(out, "done") {
		t.Fatalf("stdout = %q", out)
	}
}
