package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChatStreamWritesErrorFrameBeforeFirstToken verifies that a streaming
// failure before the first client-visible event still produces an SSE error
// frame instead of silently closing an empty 200 response.
func TestChatStreamWritesErrorFrameBeforeFirstToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// End the body without any SSE event and without [DONE]; the provider
		// must surface io.ErrUnexpectedEOF after headers are committed.
	}))
	defer upstream.Close()

	server := newTestServer(upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, upstreamErrorMessage) || !strings.Contains(body, `"type":"upstream_error"`) {
		t.Fatalf("pre-first-token failure must write an SSE error frame, got body: %q", body)
	}
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("chat error frame must start with a data: line, got: %q", body)
	}
	if strings.Contains(body, `\n`) {
		t.Fatalf("SSE frames must use real newlines, got: %q", body)
	}
}

// TestResponsesStreamWritesErrorFrameBeforeFirstToken is the Responses API
// counterpart and also pins the event:/data: framing of the error frame.
func TestResponsesStreamWritesErrorFrameBeforeFirstToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	server := newTestServer(upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"chat","input":"hi","stream":true}`))
	response := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, upstreamErrorMessage) || !strings.Contains(body, `"type":"upstream_error"`) {
		t.Fatalf("pre-first-token failure must write an SSE error frame, got body: %q", body)
	}
	if !strings.HasPrefix(body, "event: error\n") {
		t.Fatalf("responses error frame must start with event: error + newline, got: %q", body)
	}
	if strings.Contains(body, `\n`) {
		t.Fatalf("SSE frames must use real newlines, got: %q", body)
	}
}
