package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/light-llm-gateway/internal/routing"
)

func TestOpenAIResponsesForwardsModelAndParsesUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "provider-model" {
			t.Fatalf("model = %v", body["model"])
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","model":"provider-model","output":[],"usage":{"input_tokens":10,"output_tokens":4,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`))
	}))
	defer upstream.Close()
	result, err := NewClient().Do(context.Background(), Request{Operation: Responses, Body: json.RawMessage(`{"model":"alias","input":"hi"}`)}, routing.Candidate{BaseURL: upstream.URL, Model: "provider-model"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage != (Usage{InputTokens: 10, OutputTokens: 4, CacheReadTokens: 3, ReasoningTokens: 2}) {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestOpenAIResponsesStreamForwardsEventsAndCompletes(t *testing.T) {
	var request map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		writeSSE(w, f, map[string]string{"event": "response.output_text.delta", "data": `{"type":"response.output_text.delta","delta":"hi"}`})
		writeSSE(w, f, map[string]string{"event": "response.completed", "data": `{"type":"response.completed","response":{"model":"provider-model","usage":{"input_tokens":5,"output_tokens":2}}}`})
	}))
	defer upstream.Close()
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: Responses, Body: json.RawMessage(`{"model":"alias","input":"hi","stream":true}`)}, routing.Candidate{BaseURL: upstream.URL, Model: "provider-model"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	if first.Event != "response.output_text.delta" || string(first.Data) != `{"type":"response.output_text.delta","delta":"hi"}` {
		t.Fatalf("event = %+v", first)
	}
	done, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !done.Done || done.Event != "response.completed" || done.Usage.InputTokens != 5 || done.Usage.OutputTokens != 2 {
		t.Fatalf("done = %+v", done)
	}
	if _, ok := request["stream_options"]; ok {
		t.Fatal("Responses request unexpectedly has stream_options")
	}
}

func TestOpenAIResponsesStreamRequiresTerminalEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, w.(http.Flusher), map[string]string{"event": "response.created", "data": `{"type":"response.created"}`})
	}))
	defer upstream.Close()
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: Responses, Body: json.RawMessage(`{"model":"m","input":"hi","stream":true}`)}, routing.Candidate{BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v", err)
	}
}
