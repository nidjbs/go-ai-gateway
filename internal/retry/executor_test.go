package retry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/circuitbreaker"
	"github.com/nidjbs/go-ai-gateway/internal/config"
	"github.com/nidjbs/go-ai-gateway/internal/provider"
	"github.com/nidjbs/go-ai-gateway/internal/routing"
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

func TestBreakerErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool // true = counted as a provider failure by the breaker
	}{
		{"nil is success", nil, false},
		{"network counted", &provider.ProviderError{Kind: provider.ErrorKindNetwork}, true},
		{"timeout counted", &provider.ProviderError{Kind: provider.ErrorKindTimeout}, true},
		{"protocol counted", &provider.ProviderError{Kind: provider.ErrorKindProtocol}, true},
		{"upstream 500 counted", &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 500}, true},
		{"upstream 503 counted", &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 503}, true},
		{"upstream 400 not counted", &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 400}, false},
		{"upstream 429 not counted", &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 429}, false},
		{"invalid_request not counted", &provider.ProviderError{Kind: provider.ErrorKindInvalidRequest}, false},
		{"canceled not counted", &provider.ProviderError{Kind: provider.ErrorKindCanceled}, false},
		{"context canceled not counted", context.Canceled, false},
		{"http 503 counted", &provider.HTTPError{StatusCode: 503}, true},
		{"http 400 not counted", &provider.HTTPError{StatusCode: 400}, false},
		{"plain error counted", errors.New("boom"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := breakerErr(tt.err)
			if (got != nil) != tt.want {
				t.Errorf("breakerErr(%v) counted=%v, want counted=%v", tt.err, got != nil, tt.want)
			}
		})
	}
}

// recordingBreaker captures every Record call for inspection.
type recordingBreaker struct {
	mu    sync.Mutex
	calls []error
}

func (b *recordingBreaker) Allow(string, time.Time) error { return nil }

func (b *recordingBreaker) Record(_ string, _ time.Time, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, err)
}

func TestExecuteBreakerIgnoresClientErrors(t *testing.T) {
	rb := &recordingBreaker{}
	cfg := config.RetryConfig{MaxAttemptsPerProvider: 3, RetryableStatuses: []int{503}}
	_, _, _, err := execute(context.Background(), cfg, config.FailoverConfig{}, rb, []routing.Candidate{{Name: "primary"}}, func(context.Context, routing.Candidate) (string, error) {
		return "", &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 400}
	}, func(context.Context, time.Duration) error { return nil }, func() float64 { return 0.5 })
	if err == nil {
		t.Fatal("expected error")
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if len(rb.calls) != 1 {
		t.Fatalf("records = %d, want 1", len(rb.calls))
	}
	if rb.calls[0] != nil {
		t.Errorf("record[0] = %v, want nil (client 400 must not count toward the breaker)", rb.calls[0])
	}
}

func TestExecuteBreakerTripsOnServerErrors(t *testing.T) {
	breaker := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold:         3,
		OpenDuration:             time.Minute,
		HalfOpenMaxRequests:      1,
		HalfOpenSuccessThreshold: 1,
	})
	cfg := config.RetryConfig{MaxAttemptsPerProvider: 5, RetryableStatuses: []int{503}}
	_, _, _, err := execute(context.Background(), cfg, config.FailoverConfig{}, breaker, []routing.Candidate{{Name: "primary"}}, func(context.Context, routing.Candidate) (string, error) {
		return "", &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 503}
	}, func(context.Context, time.Duration) error { return nil }, func() float64 { return 0.5 })
	if err == nil {
		t.Fatal("expected error")
	}
	if got := breaker.Allow("primary|", time.Now()); got == nil {
		t.Error("breaker should be open after 3 consecutive 5xx")
	}
}

func TestExecuteBreakerStaysClosedOnRepeatedClientErrors(t *testing.T) {
	breaker := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold:         3,
		OpenDuration:             time.Minute,
		HalfOpenMaxRequests:      1,
		HalfOpenSuccessThreshold: 1,
	})
	cfg := config.RetryConfig{MaxAttemptsPerProvider: 1, RetryableStatuses: []int{503}}
	for i := 0; i < 5; i++ {
		_, _, _, err := execute(context.Background(), cfg, config.FailoverConfig{}, breaker, []routing.Candidate{{Name: "primary"}}, func(context.Context, routing.Candidate) (string, error) {
			return "", &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 400}
		}, func(context.Context, time.Duration) error { return nil }, func() float64 { return 0.5 })
		if err == nil {
			t.Fatal("expected error")
		}
	}
	if got := breaker.Allow("primary|", time.Now()); got != nil {
		t.Error("breaker must stay closed after repeated client 400s")
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
