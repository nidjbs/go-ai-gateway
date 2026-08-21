package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/light-llm-gateway/internal/auth"
	"example.com/light-llm-gateway/internal/config"
)

func TestGatewayDefaultsToMemoryBackendsWithoutRedis(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chat-1","object":"chat.completion","model":"provider-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	server := newTestServer(upstream.URL)
	if server.limiter == nil {
		t.Fatal("default limiter is nil")
	}
	if server.quotaStore == nil {
		t.Fatal("default quota store is nil")
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
func TestChatForwardsProviderModelAndReturnsAlias(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["model"] != "provider-model" {
			t.Fatalf("upstream model = %v, want provider-model", request["model"])
		}
		_, _ = w.Write([]byte(`{"id":"chat-1","object":"chat.completion","model":"provider-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	server := newTestServer(upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "chat" {
		t.Fatalf("response model = %v, want chat", body["model"])
	}
}

func TestResponsesForwardsProviderModelAndReturnsAlias(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["model"] != "provider-model" {
			t.Fatalf("upstream model = %v", request["model"])
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","model":"provider-model","output":[],"usage":{"input_tokens":1,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	server := newTestServer(upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"chat","input":"hi"}`))
	response := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "chat" {
		t.Fatalf("response model = %v", body["model"])
	}
}

func TestAnthropicUnsupportedRequestIsLocalBadRequest(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()

	server, err := New(Deps{Config: &config.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]config.Provider{
			"anthropic": {Type: "anthropic", BaseURL: upstream.URL, APIKey: "token"},
		},
		Aliases:  map[string]config.Alias{"chat": {Provider: "anthropic", Model: "claude-test"}},
		Retry:    config.RetryConfig{MaxAttemptsPerProvider: 1},
		Failover: config.FailoverConfig{},
	}, Authenticator: auth.NoopAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`))
	response := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || called {
		t.Fatalf("status=%d called=%v body=%s", response.Code, called, response.Body.String())
	}
}

func TestUpstreamErrorsAreSanitized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"internal-provider-secret"}}`))
	}))
	defer upstream.Close()

	server := newTestServer(upstream.URL)
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/embeddings"} {
		t.Run(endpoint, func(t *testing.T) {
			body := `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`
			if endpoint == "/v1/embeddings" {
				body = `{"model":"chat","input":"hi"}`
			}
			request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
			response := httptest.NewRecorder()
			server.HTTP.Handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "internal-provider-secret") {
				t.Fatalf("response leaked upstream detail: %s", response.Body.String())
			}
			if !strings.Contains(response.Body.String(), upstreamErrorMessage) {
				t.Fatalf("response = %s, want sanitized upstream message", response.Body.String())
			}
		})
	}
}

func TestStreamOpenErrorIsSanitized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"internal-stream-secret"}}`))
	}))
	defer upstream.Close()

	server := newTestServer(upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "internal-stream-secret") || !strings.Contains(response.Body.String(), upstreamErrorMessage) {
		t.Fatalf("unexpected response body: %s", response.Body.String())
	}
}

func newTestServer(upstreamURL string) *Server {
	server, err := New(Deps{Config: &config.Config{
		Listen:    "127.0.0.1:0",
		Providers: map[string]config.Provider{"local": {BaseURL: upstreamURL, APIKey: "upstream-token"}},
		Aliases:   map[string]config.Alias{"chat": {Provider: "local", Model: "provider-model"}},
		Retry:     config.RetryConfig{MaxAttemptsPerProvider: 1},
		Failover:  config.FailoverConfig{},
	}, Authenticator: auth.NoopAuthenticator{}})
	if err != nil {
		panic(err)
	}
	return server
}
