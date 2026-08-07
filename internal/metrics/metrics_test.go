package metrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRecorderExposesSIUMetrics(t *testing.T) {
	recorder, err := New()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-100 * time.Millisecond)
	recorder.Record(context.Background(), Request{Operation: "chat.completions", Provider: "primary", Model: "chat", UpstreamModel: "provider-chat", StartedAt: started}, Result{StatusCode: 200, ResponseModel: "provider-chat", InputTokens: 3, OutputTokens: 2, FirstTokenAt: started.Add(20 * time.Millisecond), CompletedAt: time.Now()})

	response := httptest.NewRecorder()
	recorder.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if response.Code != 200 {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	for _, name := range []string{
		"ai_gateway_llm_request_duration_seconds",
		"ai_gateway_llm_time_to_first_token_seconds",
		"ai_gateway_llm_time_per_output_token_seconds",
		"ai_gateway_llm_token_usage",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("metrics missing %q:\n%s", name, body)
		}
	}
	if !strings.Contains(body, `operation="chat.completions"`) || !strings.Contains(body, `provider="primary"`) {
		t.Errorf("metrics missing SIU attributes:\n%s", body)
	}
}

func TestRecorderFailureOnlyRecordsDuration(t *testing.T) {
	recorder, err := New()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-100 * time.Millisecond)
	recorder.Record(context.Background(), Request{Operation: "embeddings", Provider: "primary", Model: "embedding", UpstreamModel: "provider-embedding", StartedAt: started}, Result{StatusCode: 502, ErrorType: "upstream_error", CompletedAt: time.Now()})

	response := httptest.NewRecorder()
	recorder.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	body := response.Body.String()
	if !strings.Contains(body, `success="false"`) || !strings.Contains(body, `error_type="upstream_error"`) {
		t.Errorf("failure attributes missing:\n%s", body)
	}
	if strings.Contains(body, "ai_gateway_llm_token_usage_token") {
		t.Errorf("failure should not record token usage:\n%s", body)
	}
}
