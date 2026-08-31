package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockGateway serves the OpenAI-compatible chat/models endpoints plus the
// admin surface, mirroring go-ai-gateway's wire contract.
func mockGateway(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "chat"}, {"id": "trans"}}})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if stream, _ := req["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "你好"}}},
		})
	})
	mux.HandleFunc("/admin/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "reloaded"})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/admin/usage/summary", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"total_tokens": 42})
	})
	return httptest.NewServer(mux)
}

func testClient(t *testing.T, cfg *Config) *Client {
	t.Helper()
	return NewClient(cfg)
}

func TestModels(t *testing.T) {
	srv := mockGateway(t)
	defer srv.Close()
	client := testClient(t, &Config{GatewayURL: srv.URL, APIKey: "sk-test"})

	names, err := client.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "chat" || names[1] != "trans" {
		t.Fatalf("names = %v", names)
	}
}

func TestModelsUnauthorized(t *testing.T) {
	srv := mockGateway(t)
	defer srv.Close()
	client := testClient(t, &Config{GatewayURL: srv.URL})

	if _, err := client.Models(context.Background()); err == nil {
		t.Fatal("expected auth error, got nil")
	}
}

func TestAgentTurnStreamAccumulates(t *testing.T) {
	frame := func(v any) string {
		b, _ := json.Marshal(v)
		return "data: " + string(b) + "\n\n"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, frame(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "Hel"}, "finish_reason": nil}}}))
		fmt.Fprint(w, frame(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "lo"}, "finish_reason": nil}}}))
		fmt.Fprint(w, frame(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{"index": 0, "id": "call_1", "function": map[string]any{"name": "read_file", "arguments": `{"path":"`}}}}, "finish_reason": nil}}}))
		fmt.Fprint(w, frame(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{"index": 1, "id": "call_2", "function": map[string]any{"name": "write_file", "arguments": `{"path":"x`}}}}, "finish_reason": nil}}}))
		fmt.Fprint(w, frame(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{"index": 1, "function": map[string]any{"arguments": `"}`}}}}, "finish_reason": nil}}}))
		fmt.Fprint(w, frame(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}}}))
		fmt.Fprint(w, frame(map[string]any{"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7}}))
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewClient(&Config{GatewayURL: srv.URL, APIKey: "sk-test"})
	var buf bytes.Buffer
	res, err := client.AgentTurnStream(context.Background(), "req-1", "chat", []Message{{Role: "user", Content: "x"}}, agentTools(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != "Hello" || res.Content != "Hello" {
		t.Fatalf("content = %q / %q", buf.String(), res.Content)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d", len(res.ToolCalls))
	}
	if res.ToolCalls[0].ID != "call_1" || res.ToolCalls[0].Function.Name != "read_file" || res.ToolCalls[0].Function.Arguments != `{"path":"` {
		t.Fatalf("tc0 = %+v", res.ToolCalls[0])
	}
	if res.ToolCalls[1].Function.Arguments != `{"path":"x"}` {
		t.Fatalf("tc1 args = %q", res.ToolCalls[1].Function.Arguments)
	}
	if res.FinishReason != "tool_calls" || res.InputTokens != 11 || res.OutputTokens != 7 {
		t.Fatalf("finish/usage = %q %d/%d", res.FinishReason, res.InputTokens, res.OutputTokens)
	}
}

func TestChatStream(t *testing.T) {
	srv := mockGateway(t)
	defer srv.Close()
	client := testClient(t, &Config{GatewayURL: srv.URL, APIKey: "sk-test"})

	var buf bytes.Buffer
	err := client.ChatStream(context.Background(), "chat", []Message{{Role: "user", Content: "hi"}}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "你好" {
		t.Fatalf("streamed = %q, want 你好", got)
	}
}

func TestChatNonStream(t *testing.T) {
	srv := mockGateway(t)
	defer srv.Close()
	client := testClient(t, &Config{GatewayURL: srv.URL, APIKey: "sk-test"})

	out, err := client.Chat(context.Background(), "chat", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if out != "你好" {
		t.Fatalf("out = %q, want 你好", out)
	}
}

func TestReloadAndStatus(t *testing.T) {
	srv := mockGateway(t)
	defer srv.Close()
	client := testClient(t, &Config{GatewayURL: srv.URL, AdminToken: "admin-tok"})

	if err := client.Reload(context.Background(), "/tmp/x.yaml"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := client.Status(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
}

func TestUsage(t *testing.T) {
	srv := mockGateway(t)
	defer srv.Close()
	client := testClient(t, &Config{GatewayURL: srv.URL, AdminToken: "admin-tok"})

	body, err := client.Usage(context.Background(), "chat", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(body), []byte("42")) {
		t.Fatalf("usage body = %s", body)
	}
}
