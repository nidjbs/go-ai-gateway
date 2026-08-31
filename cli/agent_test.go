package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// agentMock scripts a non-stream chat: call 1 asks to read filePath, call 2
// replies done. It records every X-Request-Id header.
func agentMock(t *testing.T, filePath string) (*httptest.Server, *[]string) {
	t.Helper()
	var reqIDs []string
	call := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		reqIDs = append(reqIDs, r.Header.Get("X-Request-Id"))
		call++
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			args, _ := json.Marshal(map[string]string{"path": filePath})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role":       "assistant",
						"content":    "",
						"tool_calls": []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": string(args)}}},
					},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 1},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"}},
		})
	})
	return httptest.NewServer(mux), &reqIDs
}

func TestAgentLoopToolThenReply(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "f.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, reqIDs := agentMock(t, filePath)
	defer srv.Close()

	cfg := &Config{GatewayURL: srv.URL, APIKey: "sk-test", DefaultAlias: "chat"}
	p := newTestPolicy(t, root)
	p.confirm = func(string) bool { return true }
	log, err := StartSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	got, reply, err := agentReply(cfg, "chat", []Message{{Role: "user", Content: "read f.txt"}}, p, log, agentTools())
	if err != nil {
		t.Fatal(err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q", reply)
	}
	// history: user + assistant(tool_calls) + tool + assistant(done)
	if len(got) != 4 {
		t.Fatalf("history len = %d, want 4: %+v", len(got), got)
	}
	if got[1].Role != "assistant" || len(got[1].ToolCalls) != 1 {
		t.Fatalf("turn 2 = %+v", got[1])
	}
	if got[2].Role != "tool" || got[2].ToolCallID != "call_1" || got[2].Content != "hello" {
		t.Fatalf("turn 3 = %+v", got[2])
	}
	if got[3].Role != "assistant" || got[3].Content != "done" {
		t.Fatalf("turn 4 = %+v", got[3])
	}
	if len(*reqIDs) != 2 {
		t.Fatalf("requests = %d, want 2", len(*reqIDs))
	}
	for _, id := range *reqIDs {
		if len(id) != 32 {
			t.Fatalf("reqID = %q len %d", id, len(id))
		}
	}
}

func TestAgentLoopMaxTurns(t *testing.T) {
	call := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		call++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []map[string]any{{"id": "call_x", "type": "function", "function": map[string]any{
						"name": "read_file", "arguments": `{"path":"/outside/root"}`,
					}}},
				},
				"finish_reason": "tool_calls",
			}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &Config{GatewayURL: srv.URL, APIKey: "sk-test", DefaultAlias: "chat"}
	p := newTestPolicy(t, t.TempDir())
	p.confirm = func(string) bool { return true }
	log, err := StartSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	if _, _, err := agentReply(cfg, "chat", []Message{{Role: "user", Content: "x"}}, p, log, agentTools()); err == nil {
		t.Fatal("expected max-turns error")
	}
	if call != maxAgentTurns {
		t.Fatalf("calls = %d, want %d", call, maxAgentTurns)
	}
}
