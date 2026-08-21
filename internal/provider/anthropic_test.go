package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/light-llm-gateway/internal/routing"
)

func TestAnthropicRequestAndResponse(t *testing.T) {
	var received struct {
		Model     string `json:"model"`
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
		ToolChoice map[string]string `json:"tool_choice"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-API-Key") != "anthropic-token" || r.Header.Get("Anthropic-Version") != anthropicVersion {
			t.Fatalf("unexpected upstream request: path=%s headers=%v", r.URL.Path, r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"tool_1","name":"lookup","input":{"city":"Shanghai"}}],"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	body := json.RawMessage(`{"model":"chat","max_tokens":200,"messages":[{"role":"system","content":"be concise"},{"role":"user","content":"weather"}],"tools":[{"type":"function","function":{"name":"lookup","description":"find weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],"tool_choice":"required"}`)
	result, err := NewClient().Do(context.Background(), Request{Operation: ChatCompletions, Body: body}, routing.Candidate{Type: "anthropic", Model: "claude-test", BaseURL: upstream.URL, APIKey: "anthropic-token"})
	if err != nil {
		t.Fatal(err)
	}
	if received.Model != "claude-test" || received.System != "be concise" || received.MaxTokens != 200 || received.ToolChoice["type"] != "any" || len(received.Tools) != 1 {
		t.Fatalf("unexpected translated request: %+v", received)
	}
	var response map[string]any
	if err := json.Unmarshal(result.Body, &response); err != nil {
		t.Fatal(err)
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 5 {
		t.Fatalf("unexpected translated response: %+v", response)
	}
	message := choice["message"].(map[string]any)
	if len(message["tool_calls"].([]any)) != 1 {
		t.Fatalf("tool call missing: %+v", message)
	}
}

func TestAnthropicRejectsUnsupportedRequestBeforeUpstream(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()
	_, err := NewClient().Do(context.Background(), Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)}, routing.Candidate{Type: "anthropic", Model: "claude-test", BaseURL: upstream.URL})
	if err == nil || !strings.Contains(err.Error(), "only text") || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestAnthropicToolResultMapping(t *testing.T) {
	body, err := anthropicRequest(json.RawMessage(`{"model":"chat","messages":[{"role":"user","content":"call"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Shanghai\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"26 degrees"}]}`), "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	var translated anthropicWireRequest
	if err := json.Unmarshal(body, &translated); err != nil {
		t.Fatal(err)
	}
	if len(translated.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(translated.Messages))
	}
	content, ok := translated.Messages[2].Content.([]any)
	if !ok || len(content) != 1 || content[0].(map[string]any)["type"] != "tool_result" {
		t.Fatalf("unexpected tool result: %#v", translated.Messages[2].Content)
	}
}

func TestAnthropicStreamMapsTextAndUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, event := range []string{
			`event: message_start\ndata: {"type":"message_start","message":{"model":"claude-test","usage":{"input_tokens":4}}}`,
			`event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`,
			`event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`event: message_stop\ndata: {"type":"message_stop"}`,
		} {
			for _, line := range strings.Split(event, `\n`) {
				_, _ = w.Write([]byte(line + "\n"))
			}
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
		}
	}))
	defer upstream.Close()
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)}, routing.Candidate{Type: "anthropic", Model: "claude-test", BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var textSeen bool
	var done StreamEvent
	for {
		event, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if event.Done {
			done = event
			break
		}
		if strings.Contains(string(event.Data), "hello") {
			textSeen = true
		}
	}
	if !textSeen || done.Usage.InputTokens != 4 || done.Usage.OutputTokens != 3 {
		t.Fatalf("text=%v done=%+v", textSeen, done)
	}
}

func decodeAnthropicWire(t *testing.T, body []byte) anthropicWireRequest {
	t.Helper()
	var wire anthropicWireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

// feedAnthropicEvents writes a sequence of Anthropic SSE frames to w.
// Each frame is encoded with both an "event:" and "data:" line followed by
// a blank terminator.
func feedAnthropicEvents(t *testing.T, w http.ResponseWriter, flusher http.Flusher, events []string) {
	t.Helper()
	for _, evt := range events {
		_, _ = fmt.Fprintf(w, "event: %s\n", evt)
	}
}

func TestAnthropicStopSequenceString(t *testing.T) {
	body, err := anthropicRequest(json.RawMessage(`{"model":"chat","stop":"END","messages":[{"role":"user","content":"hi"}]}`), "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeAnthropicWire(t, body)
	if len(wire.StopSequences) != 1 || wire.StopSequences[0] != "END" {
		t.Fatalf("stop_sequences = %#v", wire.StopSequences)
	}
}

func TestAnthropicStopSequenceArray(t *testing.T) {
	body, err := anthropicRequest(json.RawMessage(`{"model":"chat","stop":["END","STOP","###"],"messages":[{"role":"user","content":"hi"}]}`), "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeAnthropicWire(t, body)
	if len(wire.StopSequences) != 3 || wire.StopSequences[1] != "STOP" {
		t.Fatalf("stop_sequences = %#v", wire.StopSequences)
	}
}

func TestAnthropicStopSequenceTooMany(t *testing.T) {
	_, err := anthropicRequest(json.RawMessage(`{"model":"chat","stop":["a","b","c","d","e"],"messages":[{"role":"user","content":"hi"}]}`), "claude-test", false)
	if err == nil || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("expected stop error, got %v", err)
	}
}

func TestAnthropicStopSequenceEmpty(t *testing.T) {
	_, err := anthropicRequest(json.RawMessage(`{"model":"chat","stop":"","messages":[{"role":"user","content":"hi"}]}`), "claude-test", false)
	if err == nil || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("expected stop error, got %v", err)
	}
}

func TestAnthropicTopKForwards(t *testing.T) {
	body, err := anthropicRequest(json.RawMessage(`{"model":"chat","top_k":40,"messages":[{"role":"user","content":"hi"}]}`), "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeAnthropicWire(t, body)
	if wire.TopK == nil || *wire.TopK != 40 {
		t.Fatalf("top_k = %v, want 40", wire.TopK)
	}
}

func TestAnthropicSeedForwards(t *testing.T) {
	body, err := anthropicRequest(json.RawMessage(`{"model":"chat","seed":12345,"messages":[{"role":"user","content":"hi"}]}`), "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeAnthropicWire(t, body)
	if wire.Seed == nil || *wire.Seed != 12345 {
		t.Fatalf("seed = %v, want 12345", wire.Seed)
	}
}

func TestAnthropicThinkingForwards(t *testing.T) {
	thinking := json.RawMessage(`{"type":"enabled","budget_tokens":2048}`)
	body, err := anthropicRequest(json.RawMessage(`{"model":"chat","thinking":`+string(thinking)+`,"messages":[{"role":"user","content":"hi"}]}`), "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeAnthropicWire(t, body)
	var decoded map[string]any
	if err := json.Unmarshal(wire.Thinking, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["type"] != "enabled" || int(decoded["budget_tokens"].(float64)) != 2048 {
		t.Fatalf("thinking = %#v", decoded)
	}
}

func TestAnthropicParallelToolCallsTrueAccepted(t *testing.T) {
	body, err := anthropicRequest(json.RawMessage(`{"model":"chat","parallel_tool_calls":true,"messages":[{"role":"user","content":"hi"}]}`), "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeAnthropicWire(t, body)
	if len(wire.Messages) == 0 {
		t.Fatalf("messages missing: %#v", wire)
	}
}

func TestAnthropicParallelToolCallsFalseRejected(t *testing.T) {
	_, err := anthropicRequest(json.RawMessage(`{"model":"chat","parallel_tool_calls":false,"messages":[{"role":"user","content":"hi"}]}`), "claude-test", false)
	if err == nil || !strings.Contains(err.Error(), "parallel_tool_calls") {
		t.Fatalf("expected parallel_tool_calls error, got %v", err)
	}
}

func TestAnthropicRejectsNewUnsupported(t *testing.T) {
	cases := map[string]string{
		"user":              `"user-123"`,
		"presence_penalty":  `0.5`,
		"frequency_penalty": `0.5`,
		"logprobs":          `true`,
		"top_logprobs":      `5`,
		"service_tier":      `"auto"`,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"chat","%s":%s,"messages":[{"role":"user","content":"hi"}]}`, name, value)
			_, err := anthropicRequest(json.RawMessage(body), "claude-test", false)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("expected %s error, got %v", name, err)
			}
		})
	}
}

func TestAnthropicAllNewParamsCombined(t *testing.T) {
	body, err := anthropicRequest(json.RawMessage(`{"model":"chat","temperature":0.7,"top_p":0.9,"top_k":50,"seed":7,"stop":["END"],"messages":[{"role":"user","content":"hi"}]}`), "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeAnthropicWire(t, body)
	if wire.Temperature == nil || *wire.Temperature != 0.7 {
		t.Fatalf("temperature = %v", wire.Temperature)
	}
	if wire.TopP == nil || *wire.TopP != 0.9 {
		t.Fatalf("top_p = %v", wire.TopP)
	}
	if wire.TopK == nil || *wire.TopK != 50 {
		t.Fatalf("top_k = %v", wire.TopK)
	}
	if wire.Seed == nil || *wire.Seed != 7 {
		t.Fatalf("seed = %v", wire.Seed)
	}
	if len(wire.StopSequences) != 1 || wire.StopSequences[0] != "END" {
		t.Fatalf("stop_sequences = %#v", wire.StopSequences)
	}
}

// writeAnthropicFrame writes a single Anthropic SSE frame with the given
// event type and JSON payload, followed by a blank terminator.
func writeAnthropicFrame(w http.ResponseWriter, flusher http.Flusher, event string, payload string) {
	_, _ = fmt.Fprintf(w, "event: %s\n", event)
	_, _ = fmt.Fprintf(w, "data: %s\n", payload)
	_, _ = w.Write([]byte("\n"))
	flusher.Flush()
}

func TestAnthropicStreamAccumulatesToolCallJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeAnthropicFrame(w, flusher, "message_start", `{"type":"message_start","message":{"model":"claude","usage":{"input_tokens":2}}}`)
		writeAnthropicFrame(w, flusher, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup"}}`)
		writeAnthropicFrame(w, flusher, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`)
		writeAnthropicFrame(w, flusher, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Shanghai\"}"}}`)
		writeAnthropicFrame(w, flusher, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		writeAnthropicFrame(w, flusher, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`)
		writeAnthropicFrame(w, flusher, "message_stop", `{"type":"message_stop"}`)
	}))
	defer upstream.Close()
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"weather"}]}`)}, routing.Candidate{Type: "anthropic", Model: "claude", BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var sawStart, sawDelta1, sawDelta2 bool
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
		body := string(ev.Data)
		switch {
		case strings.Contains(body, `"id":"toolu_1"`) && strings.Contains(body, `"name":"lookup"`):
			sawStart = true
		case strings.Contains(body, `city`) && strings.Contains(body, `Shanghai`) == false:
			sawDelta1 = true
		case strings.Contains(body, `Shanghai`):
			sawDelta2 = true
		}
	}
	if !sawStart || !sawDelta1 || !sawDelta2 {
		t.Fatalf("flags: start=%v d1=%v d2=%v", sawStart, sawDelta1, sawDelta2)
	}
	if done.Usage.OutputTokens != 5 || done.Usage.InputTokens != 2 {
		t.Fatalf("usage = %+v", done)
	}
}

func TestAnthropicStreamMergesCacheUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeAnthropicFrame(w, flusher, "message_start", `{"type":"message_start","message":{"model":"claude","usage":{"input_tokens":100,"cache_read_input_tokens":30,"cache_creation_input_tokens":12}}}`)
		writeAnthropicFrame(w, flusher, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4,"cache_read_input_tokens":50}}`)
		writeAnthropicFrame(w, flusher, "message_stop", `{"type":"message_stop"}`)
	}))
	defer upstream.Close()
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)}, routing.Candidate{Type: "anthropic", Model: "claude", BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		ev, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Done {
			// Token counts visible to the gateway come from the standard fields.
			if ev.Usage.InputTokens != 100 || ev.Usage.OutputTokens != 4 {
				t.Fatalf("usage = %+v", ev)
			}
			break
		}
	}
}

func TestAnthropicStreamFallsBackToCandidateModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// message_start omits the model field; the candidate model must be used.
		writeAnthropicFrame(w, flusher, "message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`)
		writeAnthropicFrame(w, flusher, "message_stop", `{"type":"message_stop"}`)
	}))
	defer upstream.Close()
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)}, routing.Candidate{Type: "anthropic", Model: "fallback-model", BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var modelSeen string
	for {
		ev, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Done {
			break
		}
		var peek struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(ev.Data, &peek)
		if peek.Model != "" {
			modelSeen = peek.Model
		}
	}
	if modelSeen != "fallback-model" {
		t.Fatalf("model = %q, want fallback-model", modelSeen)
	}
}

func TestAnthropicStreamPropagatesErrorEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeAnthropicFrame(w, flusher, "error", `{"type":"error","error":{"type":"overloaded_error","message":"upstream overloaded"}}`)
	}))
	defer upstream.Close()
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)}, routing.Candidate{Type: "anthropic", Model: "claude", BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = stream.Next()
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T %v", err, err)
	}
	if pe.Kind != ErrorKindUpstream {
		t.Fatalf("kind = %q; want upstream", pe.Kind)
	}
	if !strings.Contains(pe.Message, "overloaded") {
		t.Fatalf("err message = %q", pe.Message)
	}
}

func TestAnthropicStreamIgnoresPingEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeAnthropicFrame(w, flusher, "message_start", `{"type":"message_start","message":{"model":"claude","usage":{"input_tokens":1}}}`)
		writeAnthropicFrame(w, flusher, "ping", `{"type":"ping"}`)
		writeAnthropicFrame(w, flusher, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`)
		writeAnthropicFrame(w, flusher, "ping", `{"type":"ping"}`)
		writeAnthropicFrame(w, flusher, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)
		writeAnthropicFrame(w, flusher, "message_stop", `{"type":"message_stop"}`)
	}))
	defer upstream.Close()
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)}, routing.Candidate{Type: "anthropic", Model: "claude", BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	pingFiltered := true
	for {
		ev, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Done {
			break
		}
		if strings.Contains(string(ev.Data), `"type":"ping"`) {
			pingFiltered = false
		}
	}
	if !pingFiltered {
		t.Fatal("ping event was forwarded to caller")
	}
}

func TestAnthropicStreamForwardsReasoningDelta(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeAnthropicFrame(w, flusher, "message_start", `{"type":"message_start","message":{"model":"claude","usage":{"input_tokens":1}}}`)
		writeAnthropicFrame(w, flusher, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}`)
		writeAnthropicFrame(w, flusher, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`)
		writeAnthropicFrame(w, flusher, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`)
		writeAnthropicFrame(w, flusher, "message_stop", `{"type":"message_stop"}`)
	}))
	defer upstream.Close()
	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)}, routing.Candidate{Type: "anthropic", Model: "claude", BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var sawReasoning bool
	for {
		ev, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Done {
			break
		}
		var peek struct {
			Choices []struct {
				Delta struct {
					Reasoning string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(ev.Data, &peek)
		for _, c := range peek.Choices {
			if c.Delta.Reasoning != "" {
				sawReasoning = true
			}
		}
	}
	if !sawReasoning {
		t.Fatal("reasoning delta not forwarded")
	}
}

// TestAnthropicResultPropagatesCacheTokens verifies that cache_creation_input_tokens
// and cache_read_input_tokens on the Anthropic wire response are surfaced in
// Result.Usage so they can be plumbed into audit and metrics.
func TestAnthropicResultPropagatesCacheTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":80,"cache_creation_input_tokens":25}}`))
	}))
	defer upstream.Close()

	result, err := NewClient().Do(context.Background(),
		Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`)},
		routing.Candidate{Type: "anthropic", Model: "claude-test", BaseURL: upstream.URL, APIKey: "x"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.InputTokens != 10 {
		t.Fatalf("InputTokens = %d", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 4 {
		t.Fatalf("OutputTokens = %d", result.Usage.OutputTokens)
	}
	if result.Usage.CacheReadTokens != 80 {
		t.Fatalf("CacheReadTokens = %d", result.Usage.CacheReadTokens)
	}
	if result.Usage.CacheCreationTokens != 25 {
		t.Fatalf("CacheCreationTokens = %d", result.Usage.CacheCreationTokens)
	}
	// Total must exclude cache tokens (they are discounted/reused context).
	if got, want := result.Usage.Total(), 14; got != want {
		t.Fatalf("Total() = %d, want %d", got, want)
	}
}

// TestAnthropicStreamPropagatesCacheTokensOnMessageStop verifies that
// cache_read/cache_creation values reported across message_start/message_delta
// surface in the final StreamEvent.Usage on message_stop.
func TestAnthropicStreamPropagatesCacheTokensOnMessageStop(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeAnthropicFrame(w, flusher, "message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":7,"cache_read_input_tokens":50,"cache_creation_input_tokens":10}}}`)
		writeAnthropicFrame(w, flusher, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		writeAnthropicFrame(w, flusher, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
		writeAnthropicFrame(w, flusher, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3,"cache_read_input_tokens":50}}`)
		writeAnthropicFrame(w, flusher, "message_stop", `{"type":"message_stop"}`)
		flusher.Flush()
	}))
	defer upstream.Close()

	stream, err := NewClient().OpenStream(context.Background(),
		Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)},
		routing.Candidate{Type: "anthropic", Model: "claude", BaseURL: upstream.URL, APIKey: "x"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var done *StreamEvent
	for {
		ev, err := stream.Next()
		if err != nil {
			t.Fatalf("stream.Next: %v", err)
		}
		if ev.Done {
			done = &ev
			break
		}
	}
	if done == nil {
		t.Fatal("stream ended without message_stop")
	}
	if done.Usage.InputTokens != 7 {
		t.Fatalf("InputTokens = %d", done.Usage.InputTokens)
	}
	if done.Usage.OutputTokens != 3 {
		t.Fatalf("OutputTokens = %d", done.Usage.OutputTokens)
	}
	if done.Usage.CacheReadTokens != 50 {
		t.Fatalf("CacheReadTokens = %d", done.Usage.CacheReadTokens)
	}
	if done.Usage.CacheCreationTokens != 10 {
		t.Fatalf("CacheCreationTokens = %d", done.Usage.CacheCreationTokens)
	}
}

// TestAnthropicAggregatesConsecutiveToolMessages verifies that multiple
// consecutive "tool" messages from the OpenAI-style request are collapsed into
// a single Anthropic user message whose content is a list of tool_result
// blocks.
func TestAnthropicAggregatesConsecutiveToolMessages(t *testing.T) {
	body, err := anthropicRequest(json.RawMessage(`{"model":"chat","messages":[
		{"role":"user","content":"weather"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Shanghai\"}"}},
			{"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Beijing\"}"}}
		]},
		{"role":"tool","tool_call_id":"call_1","content":"26C"},
		{"role":"tool","tool_call_id":"call_2","content":"22C"}
	]}`), "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeAnthropicWire(t, body)
	if len(wire.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, aggregated-tool-user)", len(wire.Messages))
	}
	if wire.Messages[2].Role != "user" {
		t.Fatalf("aggregated message role = %q, want user", wire.Messages[2].Role)
	}
	content, ok := wire.Messages[2].Content.([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("aggregated content = %#v, want 2 tool_result blocks", wire.Messages[2].Content)
	}
	for i, block := range content {
		m, ok := block.(map[string]any)
		if !ok || m["type"] != "tool_result" {
			t.Fatalf("block[%d] = %#v", i, block)
		}
	}
	ids := map[string]bool{
		content[0].(map[string]any)["tool_use_id"].(string): true,
		content[1].(map[string]any)["tool_use_id"].(string): true,
	}
	if !ids["call_1"] || !ids["call_2"] {
		t.Fatalf("tool_use_ids = %+v, want call_1+call_2", ids)
	}
}

// TestAnthropicKeepsSeparateToolMessagesWhenNonToolBetween verifies that a
// non-tool message between two tool messages breaks aggregation into two
// distinct Anthropic user messages.
func TestAnthropicKeepsSeparateToolMessagesWhenNonToolBetween(t *testing.T) {
	body, err := anthropicRequest(json.RawMessage(`{"model":"chat","messages":[
		{"role":"user","content":"a"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"call_2","type":"function","function":{"name":"f","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"call_1","content":"r1"},
		{"role":"user","content":"between"},
		{"role":"tool","tool_call_id":"call_2","content":"r2"}
	]}`), "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeAnthropicWire(t, body)
	if len(wire.Messages) != 5 {
		t.Fatalf("messages = %d, want 5", len(wire.Messages))
	}
	for _, i := range []int{0, 2, 3, 4} {
		if wire.Messages[i].Role != "user" {
			t.Fatalf("messages[%d].Role = %q, want user", i, wire.Messages[i].Role)
		}
	}
}

// TestAnthropicRejectsToolChoiceReferencingUnknownTool verifies that a
// function-type tool_choice whose name is not in the tools array is rejected
// with a specific message rather than a generic "unsupported tool_choice".
func TestAnthropicRejectsToolChoiceReferencingUnknownTool(t *testing.T) {
	body := `{"model":"chat","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"alpha","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"beta"}}}`
	_, err := anthropicRequest(json.RawMessage(body), "claude-test", false)
	if err == nil {
		t.Fatal("expected tool_choice error")
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Fatalf("error should name the missing tool; got %v", err)
	}
}

// TestAnthropicRejectsToolChoiceRequiredWithoutTools verifies that
// tool_choice="required" with no tools array is rejected.
func TestAnthropicRejectsToolChoiceRequiredWithoutTools(t *testing.T) {
	_, err := anthropicRequest(json.RawMessage(`{"model":"chat","messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`), "claude-test", false)
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required-without-tools error, got %v", err)
	}
}

// TestAnthropicRejectsInvalidToolChoiceStrings covers the new error messages
// for unknown string choices and empty function names.
func TestAnthropicRejectsInvalidToolChoiceStrings(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unknown string", `{"model":"chat","messages":[{"role":"user","content":"hi"}],"tool_choice":"something"}`},
		{"empty function name", `{"model":"chat","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a","parameters":{"type":"object"}}}],` +
			`"tool_choice":{"type":"function","function":{"name":"  "}}}`},
		{"wrong type", `{"model":"chat","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"any","function":{"name":"a"}}}`},
		{"malformed object", `{"model":"chat","messages":[{"role":"user","content":"hi"}],"tool_choice":123}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := anthropicRequest(json.RawMessage(tc.body), "claude-test", false)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "tool_choice") {
				t.Fatalf("error should mention tool_choice; got %v", err)
			}
		})
	}
}

// TestAnthropicRejectsDuplicateToolNames verifies that two tools sharing the
// same name are rejected before the wire request is produced.
func TestAnthropicRejectsDuplicateToolNames(t *testing.T) {
	body := `{"model":"chat","messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"function","function":{"name":"dup","parameters":{"type":"object"}}},
		{"type":"function","function":{"name":"dup","parameters":{"type":"object"}}}
	]}`
	_, err := anthropicRequest(json.RawMessage(body), "claude-test", false)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-tool-name error, got %v", err)
	}
}

// TestAnthropicRejectsToolWithoutParameters verifies that a tool definition
// without a parameters schema is rejected.
func TestAnthropicRejectsToolWithoutParameters(t *testing.T) {
	body := `{"model":"chat","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x"}}]}`
	_, err := anthropicRequest(json.RawMessage(body), "claude-test", false)
	if err == nil || !strings.Contains(err.Error(), "x") {
		t.Fatalf("expected per-tool error mentioning name; got %v", err)
	}
}
