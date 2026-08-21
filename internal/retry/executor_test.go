package retry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"example.com/light-llm-gateway/internal/circuitbreaker"
	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/provider"
	"example.com/light-llm-gateway/internal/routing"
	"gopkg.in/yaml.v3"
)

func TestExecuteRespectsRetryAndFailover(t *testing.T) {
	candidates := []routing.Candidate{{Name: "primary"}, {Name: "backup"}}
	cases := []struct {
		name      string
		retry     bool
		failover  bool
		wantCalls []int
		wantTotal int
	}{
		{name: "retry and failover", retry: true, failover: true, wantCalls: []int{2, 2}, wantTotal: 4},
		{name: "retry disabled", retry: false, failover: true, wantCalls: []int{1, 1}, wantTotal: 2},
		{name: "failover disabled", retry: true, failover: false, wantCalls: []int{2, 0}, wantTotal: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := make([]int, len(candidates))
			cfg := retryConfig(t, tc.retry)
			failover := failoverConfig(t, tc.failover)
			_, _, attempts, err := execute(context.Background(), cfg, failover, circuitbreaker.Noop{}, candidates, func(_ context.Context, candidate routing.Candidate) (string, error) {
				index := 0
				if candidate.Name == "backup" {
					index = 1
				}
				calls[index]++
				return "", &provider.HTTPError{StatusCode: 503}
			}, func(context.Context, time.Duration) error { return nil }, func() float64 { return 0.5 })
			if err == nil {
				t.Fatal("expected error")
			}
			if attempts.Total != tc.wantTotal {
				t.Fatalf("attempts = %d, want %d", attempts.Total, tc.wantTotal)
			}
			for i, want := range tc.wantCalls {
				if calls[i] != want {
					t.Errorf("calls[%d] = %d, want %d", i, calls[i], want)
				}
			}
		})
	}
}

func TestExecuteZeroAttemptsStillCallsOnce(t *testing.T) {
	calls := 0
	_, _, _, err := execute(context.Background(), config.RetryConfig{RetryableStatuses: []int{503}}, config.FailoverConfig{}, circuitbreaker.Noop{}, []routing.Candidate{{Name: "primary"}}, func(context.Context, routing.Candidate) (string, error) {
		calls++
		return "", errors.New("failed")
	}, func(context.Context, time.Duration) error { return nil }, func() float64 { return 0.5 })
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d, err = %v; want one call and an error", calls, err)
	}
}

func TestExecuteStreamSurvivesAttemptContextCancel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
			time.Sleep(40 * time.Millisecond)
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()

	cfg := retryConfig(t, true)
	cfg.PerAttemptTimeout = 30 * time.Millisecond
	candidates := []routing.Candidate{{Name: "primary", BaseURL: upstream.URL, Timeout: 2 * time.Second}}
	body := json.RawMessage(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	stream, _, _, err := Execute[provider.Stream](context.Background(), cfg, config.FailoverConfig{}, circuitbreaker.Noop{}, candidates, func(ctx context.Context, _ routing.Candidate) (provider.Stream, error) {
		return provider.NewClient().OpenStream(ctx, provider.Request{Operation: provider.ChatCompletions, Body: body}, candidates[0])
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	defer stream.Close()

	for i := 0; i < 2; i++ {
		event, err := stream.Next()
		if err != nil {
			t.Fatalf("Next #%d: %v", i+1, err)
		}
		if event.Done || len(event.Data) == 0 {
			t.Fatalf("Next #%d: unexpected empty event %+v", i+1, event)
		}
	}
	done, err := stream.Next()
	if err != nil {
		t.Fatalf("Next DONE: %v", err)
	}
	if !done.Done {
		t.Fatalf("final event not Done: %+v", done)
	}
}

func TestBackoffDuration(t *testing.T) {
	cfg := config.RetryConfig{InitialInterval: 100 * time.Millisecond, MaxInterval: 250 * time.Millisecond, Multiplier: 2}
	for index, want := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 250 * time.Millisecond} {
		if got := backoffDuration(cfg, uint(index), func() float64 { return 0.5 }); got != want {
			t.Errorf("backoff[%d] = %s, want %s", index, got, want)
		}
	}
}

func TestBackoffDurationAppliesJitter(t *testing.T) {
	cfg := config.RetryConfig{InitialInterval: 100 * time.Millisecond, MaxInterval: 200 * time.Millisecond, Multiplier: 1, Jitter: 0.2}
	for _, tc := range []struct {
		random float64
		want   time.Duration
	}{{0, 80 * time.Millisecond}, {0.5, 100 * time.Millisecond}, {1, 120 * time.Millisecond}} {
		if got := backoffDuration(cfg, 0, func() float64 { return tc.random }); got != tc.want {
			t.Errorf("backoff with random %f = %s, want %s", tc.random, got, tc.want)
		}
	}
}

func TestRetryableProviderErrorKinds(t *testing.T) {
	cfg := config.RetryConfig{RetryableStatuses: []int{503}}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"protocol never retryable", &provider.ProviderError{Kind: provider.ErrorKindProtocol}, false},
		{"invalid_request never retryable", &provider.ProviderError{Kind: provider.ErrorKindInvalidRequest}, false},
		{"canceled never retryable", &provider.ProviderError{Kind: provider.ErrorKindCanceled}, false},
		{"network always retryable", &provider.ProviderError{Kind: provider.ErrorKindNetwork}, true},
		{"timeout always retryable", &provider.ProviderError{Kind: provider.ErrorKindTimeout}, true},
		{"upstream with retryable status", &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 503}, true},
		{"upstream with non-retryable status", &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 400}, false},
		{"unknown not retryable", &provider.ProviderError{Kind: provider.ErrorKindUnknown}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryable(tt.err, cfg); got != tt.want {
				t.Errorf("retryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestExecuteDoesNotRetryProtocolError ensures the retry executor short-circuits
// on protocol errors — malformed/unparseable upstream responses should never be retried.
func TestExecuteDoesNotRetryProtocolError(t *testing.T) {
	calls := 0
	cfg := retryConfig(t, true)
	_, _, attempts, err := execute(context.Background(), cfg, failoverConfig(t, false), circuitbreaker.Noop{}, []routing.Candidate{{Name: "primary"}}, func(context.Context, routing.Candidate) (string, error) {
		calls++
		return "", &provider.ProviderError{Kind: provider.ErrorKindProtocol, Message: "malformed response"}
	}, func(context.Context, time.Duration) error { return nil }, func() float64 { return 0.5 })
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on protocol error)", calls)
	}
	if attempts.Total != 1 {
		t.Errorf("attempts = %d, want 1", attempts.Total)
	}
}

func TestExecuteStopsWhenSleepCanceled(t *testing.T) {
	_, _, attempts, err := execute(context.Background(), retryConfig(t, true), failoverConfig(t, true), circuitbreaker.Noop{}, []routing.Candidate{{Name: "primary"}, {Name: "backup"}}, func(context.Context, routing.Candidate) (string, error) {
		return "", &provider.HTTPError{StatusCode: 503}
	}, func(context.Context, time.Duration) error { return context.Canceled }, func() float64 { return 0.5 })
	if !errors.Is(err, context.Canceled) || attempts.Total != 1 {
		t.Fatalf("attempts = %d, err = %v; want cancellation after one attempt", attempts.Total, err)
	}
}

func retryConfig(t *testing.T, enabled bool) config.RetryConfig {
	t.Helper()
	var cfg config.RetryConfig
	if err := yaml.Unmarshal([]byte("enabled: "+strconv.FormatBool(enabled)+"\nmax_attempts_per_provider: 2\nmax_elapsed_time: 1s\nretryable_statuses: [503]\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func failoverConfig(t *testing.T, enabled bool) config.FailoverConfig {
	t.Helper()
	var cfg config.FailoverConfig
	if err := yaml.Unmarshal([]byte("enabled: "+strconv.FormatBool(enabled)), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}
