package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
)

// evalFake serves an OpenAI-compatible /v1/chat/completions whose replies follow
// a per-case stub FIFO (one model request consumes one step), so snapshot output
// is byte-deterministic. It lives inside the `gw eval` process and the spawned
// CLI child connects over HTTP — no gateway needed.
type evalFake struct {
	srv      *httptest.Server
	fallback string
	mu       sync.Mutex
	steps    []StubStep
	next     int
	callID   int
}

func newEvalFake(stub []StubStep, fallback string) *evalFake {
	if fallback == "" {
		fallback = "好的。"
	}
	f := &evalFake{steps: stub, fallback: fallback}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		var step StubStep
		if f.next < len(f.steps) {
			step = f.steps[f.next]
			f.next++
		} else {
			step = StubStep{Text: f.fallback}
		}
		f.callID++
		id := "call_eval_" + strconv.Itoa(f.callID)
		f.mu.Unlock()

		if step.ToolCall != nil {
			f.serveToolCall(w, req.Stream, id, *step.ToolCall)
			return
		}
		f.serveText(w, req.Stream, step.Text)
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *evalFake) URL() string { return f.srv.URL }
func (f *evalFake) Close()      { f.srv.Close() }

// sse writes one data: JSON event.
func sse(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func (f *evalFake) serveText(w http.ResponseWriter, stream bool, text string) {
	if !stream {
		writeChatJSON(w, map[string]any{"content": text}, "stop")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	sse(w, map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": text},
	}}})
	sse(w, map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{}, "finish_reason": "stop",
	}}})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (f *evalFake) serveToolCall(w http.ResponseWriter, stream bool, id string, tool StubTool) {
	argJSON, _ := json.Marshal(tool.Arguments)
	if !stream {
		writeChatJSON(w, map[string]any{"content": "", "tool_calls": []any{map[string]any{
			"id": id, "type": "function",
			"function": map[string]string{"name": tool.Name, "arguments": string(argJSON)},
		}}}, "tool_calls")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	sse(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": id, "type": "function",
			"function": map[string]string{"name": tool.Name, "arguments": string(argJSON)},
		}},
	}}}})
	sse(w, map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{}, "finish_reason": "tool_calls",
	}}})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// writeChatJSON encodes the non-streaming OpenAI chat completion response.
func writeChatJSON(w http.ResponseWriter, message map[string]any, finish string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": message, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
	})
}
