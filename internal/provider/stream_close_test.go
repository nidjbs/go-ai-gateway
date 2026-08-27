package provider

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestStreamCloseIsSafeUnderConcurrentInvocation pins the Close contract:
// the gateway closes streams from both the pump cleanup goroutine and the
// handler's deferred Close, so Close must be idempotent and race-free.
// Run with -race for the assertion to be meaningful.
func TestStreamCloseIsSafeUnderConcurrentInvocation(t *testing.T) {
	tests := []struct {
		name   string
		stream Stream
	}{
		{"openai", &openAIStream{response: &http.Response{Body: io.NopCloser(strings.NewReader(""))}, cancel: func() {}}},
		{"anthropic", &anthropicStream{response: &http.Response{Body: io.NopCloser(strings.NewReader(""))}, cancel: func() {}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = tt.stream.Close()
				}()
			}
			wg.Wait()
		})
	}
}
