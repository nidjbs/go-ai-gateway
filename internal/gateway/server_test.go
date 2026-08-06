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

	server := New(Deps{Config: &config.Config{Listen: "127.0.0.1:0", Providers: map[string]config.Provider{"local": {BaseURL: upstream.URL, APIKey: "upstream-token"}}, Aliases: map[string]config.Alias{"chat": {Provider: "local", Model: "provider-model"}}, Retry: config.RetryConfig{MaxAttemptsPerProvider: 1}, Failover: config.FailoverConfig{Enabled: true}}, Authenticator: auth.NoopAuthenticator{}})
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
