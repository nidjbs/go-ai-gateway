package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

type server struct {
	role string

	mu    sync.Mutex
	stats stats
}

type stats struct {
	Role   string         `json:"role"`
	Total  int            `json:"total"`
	ByCase map[string]int `json:"by_case"`
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string `json:"role"`
		Content    any    `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Stream   bool  `json:"stream"`
	Tools    []any `json:"tools"`
	Metadata struct {
		Case string `json:"e2e_case"`
	} `json:"metadata"`
}

type messageRequest struct {
	Model    string `json:"model"`
	System   string `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
	Tools  []struct {
		Name string `json:"name"`
	} `json:"tools"`
	Metadata struct {
		Case string `json:"e2e_case"`
	} `json:"metadata"`
}

type embeddingRequest struct {
	Model    string `json:"model"`
	Input    any    `json:"input"`
	Metadata struct {
		Case string `json:"e2e_case"`
	} `json:"metadata"`
}

func main() {
	listen := flag.String("listen", "127.0.0.1:19090", "listen address")
	role := flag.String("role", "primary", "mock provider role")
	flag.Parse()

	s := &server{role: *role, stats: stats{Role: *role, ByCase: make(map[string]int)}}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/debug/stats", s.handleStats)
	mux.HandleFunc("/v1/chat/completions", s.chat)
	mux.HandleFunc("/v1/messages", s.messages)
	mux.HandleFunc("/v1/embeddings", s.embeddings)
	log.Printf("mock upstream role=%s listen=%s", *role, *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func (s *server) handleStats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = json.NewEncoder(w).Encode(s.stats)
}

func (s *server) record(caseName string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Total++
	s.stats.ByCase[caseName]++
	return s.stats.ByCase[caseName]
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	var request chatRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, `{"error":{"message":"invalid mock request"}}`, http.StatusBadRequest)
		return
	}
	caseName := request.Metadata.Case
	attempt := s.record(caseName)
	if request.Model != s.role+"-chat" || r.Header.Get("Authorization") != "Bearer "+s.role+"-token" {
		http.Error(w, `{"error":{"message":"mock request verification failed"}}`, http.StatusInternalServerError)
		return
	}

	switch caseName {
	case "tool_round_1":
		if len(request.Messages) != 1 || request.Messages[0].Role != "user" || len(request.Tools) == 0 {
			http.Error(w, `{"error":{"message":"tool request was not preserved"}}`, http.StatusInternalServerError)
			return
		}
		s.writeCompletion(w, request.Model, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_weather_1", "type": "function", "function": map[string]string{"name": "get_weather", "arguments": `{"city":"Shanghai","units":"metric"}`}}}}, "tool_calls")
	case "tool_round_2":
		if len(request.Messages) != 3 || request.Messages[1].Role != "assistant" || len(request.Messages[1].ToolCalls) != 1 || request.Messages[1].ToolCalls[0].ID != "call_weather_1" || request.Messages[2].Role != "tool" || request.Messages[2].ToolCallID != "call_weather_1" {
			http.Error(w, `{"error":{"message":"tool history was not preserved"}}`, http.StatusInternalServerError)
			return
		}
		s.writeCompletion(w, request.Model, map[string]string{"role": "assistant", "content": "tool result accepted"}, "stop")
	case "multi_turn":
		if len(request.Messages) != 3 || request.Messages[0].Role != "user" || request.Messages[1].Role != "assistant" || request.Messages[2].Role != "user" {
			http.Error(w, `{"error":{"message":"conversation history was not preserved"}}`, http.StatusInternalServerError)
			return
		}
		s.writeCompletion(w, request.Model, map[string]string{"role": "assistant", "content": "multi-turn accepted"}, "stop")
	case "chat_retry":
		if s.role == "primary" && attempt == 1 {
			s.writeFailure(w)
			return
		}
		s.writeCompletion(w, request.Model, map[string]string{"role": "assistant", "content": s.role + " retry success"}, "stop")
	case "chat_failover":
		if s.role == "primary" {
			s.writeFailure(w)
			return
		}
		s.writeCompletion(w, request.Model, map[string]string{"role": "assistant", "content": "backup failover success"}, "stop")
	case "stream_failover":
		if s.role == "primary" {
			s.writeFailure(w)
			return
		}
		s.writeStream(w, request.Model, "backup stream", false)
	case "stream_abort_after_chunk":
		s.writeStream(w, request.Model, "primary partial", true)
	case "stream_success":
		s.writeStream(w, request.Model, "stream success", false)
	default:
		if request.Stream {
			s.writeStream(w, request.Model, "mock response", false)
			return
		}
		s.writeCompletion(w, request.Model, map[string]string{"role": "assistant", "content": "mock response"}, "stop")
	}
}

func (s *server) messages(w http.ResponseWriter, r *http.Request) {
	var request messageRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, `{"error":{"message":"invalid Anthropic mock request"}}`, http.StatusBadRequest)
		return
	}
	caseName := request.Metadata.Case
	attempt := s.record(caseName)
	if request.Model != s.role+"-anthropic" || r.Header.Get("X-API-Key") != s.role+"-token" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
		http.Error(w, `{"error":{"message":"mock Anthropic request verification failed"}}`, http.StatusInternalServerError)
		return
	}
	if caseName == "anthropic_retry" && s.role == "primary" && attempt == 1 {
		s.writeFailure(w)
		return
	}
	if caseName == "anthropic_failover" && s.role == "primary" {
		s.writeFailure(w)
		return
	}
	if request.Stream {
		s.writeAnthropicStream(w, request.Model, "anthropic stream", caseName == "anthropic_stream_abort")
		return
	}
	if caseName == "anthropic_tool_round_1" {
		if len(request.Tools) != 1 {
			http.Error(w, `{"error":{"message":"Anthropic tools were not translated"}}`, http.StatusInternalServerError)
			return
		}
		s.writeMessage(w, request.Model, []any{map[string]any{"type": "tool_use", "id": "anthropic_tool_1", "name": "get_weather", "input": map[string]string{"city": "Shanghai"}}}, "tool_use")
		return
	}
	if caseName == "anthropic_tool_round_2" {
		if len(request.Messages) < 3 {
			http.Error(w, `{"error":{"message":"Anthropic tool result was not translated"}}`, http.StatusInternalServerError)
			return
		}
		s.writeMessage(w, request.Model, []any{map[string]string{"type": "text", "text": "anthropic tool result accepted"}}, "end_turn")
		return
	}
	content := "anthropic response"
	if caseName == "anthropic_failover" {
		content = "anthropic backup failover success"
	}
	if caseName == "anthropic_retry" {
		content = "anthropic retry success"
	}
	s.writeMessage(w, request.Model, []any{map[string]string{"type": "text", "text": content}}, "end_turn")
}

func (s *server) embeddings(w http.ResponseWriter, r *http.Request) {
	var request embeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, `{"error":{"message":"invalid mock request"}}`, http.StatusBadRequest)
		return
	}
	caseName := request.Metadata.Case
	s.record(caseName)
	if request.Model != s.role+"-embedding" || r.Header.Get("Authorization") != "Bearer "+s.role+"-token" {
		http.Error(w, `{"error":{"message":"mock embedding verification failed"}}`, http.StatusInternalServerError)
		return
	}
	if caseName == "embedding_failover" && s.role == "primary" {
		s.writeFailure(w)
		return
	}
	count := 1
	if inputs, ok := request.Input.([]any); ok {
		count = len(inputs)
	}
	data := make([]any, count)
	for i := range data {
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": []float64{0.1, 0.2}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": request.Model, "data": data, "usage": map[string]int{"prompt_tokens": count, "total_tokens": count}})
}

func (s *server) writeCompletion(w http.ResponseWriter, model string, message any, finishReason string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "mock-chat", "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
		"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
	})
}

func (s *server) writeMessage(w http.ResponseWriter, model string, content []any, stopReason string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_mock", "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": stopReason, "usage": map[string]int{"input_tokens": 3, "output_tokens": 2}})
}

func (s *server) writeStream(w http.ResponseWriter, model, content string, abort bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"mock-stream\",\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q}}]}\n\n", time.Now().Unix(), model, content)
	flusher.Flush()
	if abort {
		if hijacker, ok := w.(http.Hijacker); ok {
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
			}
		}
		return
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *server) writeAnthropicStream(w http.ResponseWriter, model, content string, abort bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	write := func(event string, value any) {
		data, _ := json.Marshal(value)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}
	write("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_stream", "model": model, "usage": map[string]int{"input_tokens": 3}}})
	write("content_block_delta", map[string]any{"type": "content_block_delta", "delta": map[string]string{"type": "text_delta", "text": content}})
	if abort {
		if hijacker, ok := w.(http.Hijacker); ok {
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
			}
		}
		return
	}
	write("message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 2}})
	write("message_stop", map[string]string{"type": "message_stop"})
}

func (s *server) writeFailure(w http.ResponseWriter) {
	http.Error(w, `{"error":{"message":"mock provider secret detail"}}`, http.StatusServiceUnavailable)
}
