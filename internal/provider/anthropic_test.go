package provider

import (
	"context"
	"encoding/json"
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
	if choice["finish_reason"] != "tool_calls" || result.InputTokens != 7 || result.OutputTokens != 5 {
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
	if !textSeen || done.InputTokens != 4 || done.OutputTokens != 3 {
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
