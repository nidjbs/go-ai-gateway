package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nidjbs/go-ai-gateway/internal/routing"
)

// helper that writes an SSE chunk followed by a blank line.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, fields map[string]string) {
	for k, v := range fields {
		_, _ = fmt.Fprintf(w, "%s: %s\n", k, v)
	}
	_, _ = w.Write([]byte("\n"))
	flusher.Flush()
}

func TestEnsureIncludeUsageAddsFlagWhenAbsent(t *testing.T) {
	out := ensureIncludeUsage([]byte(`{"model":"x","stream":true,"messages":[]}`))
	if !strings.Contains(string(out), `"include_usage":true`) {
		t.Fatalf("expected include_usage flag, got %s", out)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	opts, ok := parsed["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options = %+v", parsed["stream_options"])
	}
	if parsed["stream"] != true {
		t.Fatalf("stream field dropped: %+v", parsed)
	}
}

func TestEnsureIncludeUsagePreservesExistingTrueFlag(t *testing.T) {
	body := []byte(`{"model":"x","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	out := ensureIncludeUsage(body)
	if string(out) != string(body) {
		t.Fatalf("body changed: %s", out)
	}
}

func TestEnsureIncludeUsageOverridesFalseFlag(t *testing.T) {
	body := []byte(`{"model":"x","stream":true,"stream_options":{"include_usage":false},"messages":[]}`)
	out := ensureIncludeUsage(body)
	if !strings.Contains(string(out), `"include_usage":true`) {
		t.Fatalf("expected override, got %s", out)
	}
}

func TestEnsureIncludeUsageLeavesInvalidBodyAlone(t *testing.T) {
	body := []byte(`not json`)
	out := ensureIncludeUsage(body)
	if string(out) != string(body) {
		t.Fatalf("invalid body was rewritten: %s", out)
	}
}

func TestOpenAIStreamInjectsIncludeUsageOnWire(t *testing.T) {
	var captured struct {
		Stream        bool `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Model string `json:"model"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, map[string]string{"data": `[DONE]`})
	}))
	defer upstream.Close()
	body := json.RawMessage(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: body}, routing.Candidate{BaseURL: upstream.URL, Model: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !captured.Stream {
		t.Fatal("stream flag missing on wire")
	}
	if !captured.StreamOptions.IncludeUsage {
		t.Fatal("include_usage was not injected on the wire")
	}
	if captured.Model != "chat" {
		t.Fatalf("model = %q, want chat", captured.Model)
	}
}

// TestForceUsageTriStateOnWire pins the force_usage contract: unset and
// force_usage=true inject stream_options.include_usage, force_usage=false
// leaves the body untouched.
func TestForceUsageTriStateOnWire(t *testing.T) {
	run := func(t *testing.T, force *bool, wantInjected bool) {
		var captured struct {
			StreamOptions json.RawMessage `json:"stream_options"`
		}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&captured)
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w, w.(http.Flusher), map[string]string{"data": `[DONE]`})
		}))
		defer upstream.Close()
		body := json.RawMessage(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: body}, routing.Candidate{BaseURL: upstream.URL, ForceUsage: force})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		injected := len(captured.StreamOptions) > 0 && strings.Contains(string(captured.StreamOptions), `"include_usage":true`)
		if injected != wantInjected {
			t.Fatalf("force=%v injected=%v, want %v (stream_options=%s)", force, injected, wantInjected, captured.StreamOptions)
		}
	}

	t.Run("unset injects by default", func(t *testing.T) { run(t, nil, true) })
	t.Run("true forces injection", func(t *testing.T) { run(t, boolPtr(true), true) })
	t.Run("false suppresses injection", func(t *testing.T) { run(t, boolPtr(false), false) })
}

func boolPtr(v bool) *bool { return &v }

func TestOpenAIStreamParsesUsageOnlyChunk(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, map[string]string{"data": `{"id":"c1","object":"chat.completion.chunk","model":"gpt","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`})
		writeSSE(w, flusher, map[string]string{"data": `{"id":"c1","object":"chat.completion.chunk","model":"gpt","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`})
		writeSSE(w, flusher, map[string]string{"data": `[DONE]`})
	}))
	defer upstream.Close()
	body := json.RawMessage(`{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: body}, routing.Candidate{BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var done StreamEvent
	for {
		ev, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Done {
			done = ev
			break
		}
	}
	if done.Usage.InputTokens != 11 || done.Usage.OutputTokens != 7 {
		t.Fatalf("usage = (%d,%d), want (11,7)", done.Usage.InputTokens, done.Usage.OutputTokens)
	}
}

func TestOpenAIStreamForwardsContentChunks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, chunk := range []string{
			`{"id":"c","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		} {
			writeSSE(w, flusher, map[string]string{"data": chunk})
		}
	}))
	defer upstream.Close()
	body := json.RawMessage(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: body}, routing.Candidate{BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var sawRole, sawText, sawDone bool
	for {
		ev, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Done {
			sawDone = true
			break
		}
		payload := string(ev.Data)
		switch {
		case strings.Contains(payload, `"role":"assistant"`):
			sawRole = true
		case strings.Contains(payload, `"content":"hello"`):
			sawText = true
		}
	}
	if !sawRole || !sawText || !sawDone {
		t.Fatalf("role=%v text=%v done=%v", sawRole, sawText, sawDone)
	}
}

func TestOpenAIStreamUnexpectedEOF(t *testing.T) {
	// Server closes the stream without sending [DONE].
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, map[string]string{"data": `{"id":"c","object":"chat.completion.chunk","model":"m","choices":[]}`})
	}))
	defer upstream.Close()
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"m","stream":true,"messages":[]}`)}, routing.Candidate{BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	// First event arrives fine.
	if _, err := stream.Next(); err != nil {
		t.Fatal(err)
	}
	// Second call hits EOF before [DONE] -> ErrUnexpectedEOF.
	if _, err := stream.Next(); err == nil {
		t.Fatal("expected error on premature EOF")
	}
}
