package main

import (
	"bytes"
	"context"
	"encoding/json"
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
