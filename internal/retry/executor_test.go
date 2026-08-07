package retry

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

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
			_, _, attempts, err := execute(context.Background(), cfg, failover, candidates, func(_ context.Context, candidate routing.Candidate) (string, error) {
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
	_, _, _, err := execute(context.Background(), config.RetryConfig{RetryableStatuses: []int{503}}, config.FailoverConfig{}, []routing.Candidate{{Name: "primary"}}, func(context.Context, routing.Candidate) (string, error) {
		calls++
		return "", errors.New("failed")
	}, func(context.Context, time.Duration) error { return nil }, func() float64 { return 0.5 })
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d, err = %v; want one call and an error", calls, err)
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

func TestExecuteStopsWhenSleepCanceled(t *testing.T) {
	_, _, attempts, err := execute(context.Background(), retryConfig(t, true), failoverConfig(t, true), []routing.Candidate{{Name: "primary"}, {Name: "backup"}}, func(context.Context, routing.Candidate) (string, error) {
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
