package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

func BenchmarkEstimateTokens(b *testing.B) {
	body := []byte(`{"model":"chat","messages":[{"role":"user","content":"hello world this is a benchmark prompt with some length to it"}]}`)
	for i := 0; i < b.N; i++ {
		_ = estimateTokens(body)
	}
}

func BenchmarkCapRequestBodyUncapped(b *testing.B) {
	body := []byte(`{"model":"chat","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < b.N; i++ {
		_ = capRequestBody(body, 50)
	}
}

func BenchmarkReplaceModel(b *testing.B) {
	body := []byte(`{"id":"chat-1","object":"chat.completion","model":"provider-model","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	for i := 0; i < b.N; i++ {
		_ = replaceModel(body, "alias")
	}
}

func BenchmarkIdemCacheLookupMiss(b *testing.B) {
	h := handler{config: &config.Config{Server: config.ServerConfig{IdempotencyEnabled: true}}, idem: make(map[string]idemEntry)}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Idempotency-Key", "bench-key")
	rec := httptest.NewRecorder()
	for i := 0; i < b.N; i++ {
		if h.replayIdempotent(rec, req, "chat.completions", "chat", time.Time{}) {
			b.Fatal("unexpected replay hit")
		}
	}
}
