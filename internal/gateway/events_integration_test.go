package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/auth"
	"github.com/nidjbs/go-ai-gateway/internal/config"
	"github.com/nidjbs/go-ai-gateway/internal/events"
)

// TestChatEmitsEventsToWebhook drives a real chat request through the gateway
// and asserts the configured webhook receives request.started, provider.attempt,
// and request.completed.
func TestChatEmitsEventsToWebhook(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chat-1","object":"chat.completion","model":"provider-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	var mu sync.Mutex
	var received []events.Event
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev events.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		received = append(received, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer recv.Close()

	server, err := New(Deps{
		Config: &config.Config{
			Listen:    "127.0.0.1:0",
			Providers: map[string]config.Provider{"local": {BaseURL: upstream.URL, APIKey: "upstream-token"}},
			Aliases:   map[string]config.Alias{"chat": {Provider: "local", Model: "provider-model"}},
			Retry:     config.RetryConfig{MaxAttemptsPerProvider: 1},
			Events: config.EventsConfig{
				Webhooks: []config.WebhookConfig{{
					Name: "test", URL: recv.URL,
					Events:  []string{"request.started", "request.completed", "provider.attempt"},
					Timeout: 2 * time.Second,
				}},
			},
		},
		Authenticator: auth.NoopAuthenticator{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if server.closer != nil {
			_ = server.closer.Close()
		}
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
	resp := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	have := func(typ events.EventType) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range received {
			if e.Type == typ {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !(have(events.TypeRequestStarted) && have(events.TypeRequestCompleted) && have(events.TypeProviderAttempt)) {
		time.Sleep(5 * time.Millisecond)
	}
	if !have(events.TypeRequestStarted) || !have(events.TypeRequestCompleted) || !have(events.TypeProviderAttempt) {
		mu.Lock()
		defer mu.Unlock()
		types := make([]string, 0, len(received))
		for _, e := range received {
			types = append(types, string(e.Type))
		}
		t.Fatalf("missing events; received %v", types)
	}
}
