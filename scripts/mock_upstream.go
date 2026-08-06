package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chat)
	mux.HandleFunc("/v1/embeddings", embeddings)
	log.Fatal(http.ListenAndServe("127.0.0.1:19090", mux))
}

func chat(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Mock-Fail") != "" {
		http.Error(w, `{"error":{"message":"requested mock failure"}}`, http.StatusServiceUnavailable)
		return
	}
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	if request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"mock-stream\",\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"mock response\"}}]}\n\n", time.Now().Unix(), request.Model)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "mock-chat", "object": "chat.completion", "created": time.Now().Unix(), "model": request.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": "mock response"}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
	})
}

func embeddings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": request.Model, "data": []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}}}, "usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1}})
}
