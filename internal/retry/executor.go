package retry

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"time"

	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/provider"
	"example.com/light-llm-gateway/internal/routing"
)

type Attempts struct {
	Total     int
	Retries   int
	Failovers int
}

func Execute[T any](ctx context.Context, retryCfg config.RetryConfig, failoverCfg config.FailoverConfig, candidates []routing.Candidate, attempt func(context.Context, routing.Candidate) (T, error)) (T, routing.Candidate, Attempts, error) {
	return execute(ctx, retryCfg, failoverCfg, candidates, attempt, sleep, rand.Float64)
}

func execute[T any](ctx context.Context, retryCfg config.RetryConfig, failoverCfg config.FailoverConfig, candidates []routing.Candidate, attempt func(context.Context, routing.Candidate) (T, error), sleepFn func(context.Context, time.Duration) error, random func() float64) (T, routing.Candidate, Attempts, error) {
	var zero T
	var lastCandidate routing.Candidate
	var lastErr error
	attempts := Attempts{}
	limit := len(candidates)
	if failoverCfg.MaxProviders > 0 && int(failoverCfg.MaxProviders) < limit {
		limit = int(failoverCfg.MaxProviders)
	}
	maxAttempts := retryCfg.MaxAttemptsPerProvider
	if maxAttempts == 0 || !retryCfg.IsEnabled() {
		maxAttempts = 1
	}
	for i := 0; i < limit; i++ {
		if i > 0 {
			attempts.Failovers++
		}
		candidate := candidates[i]
		lastCandidate = candidate
		started := time.Now()
		for n := uint(0); n < maxAttempts; n++ {
			if n > 0 {
				attempts.Retries++
			}
			attempts.Total++
			attemptCtx := ctx
			cancel := func() {}
			if retryCfg.PerAttemptTimeout > 0 {
				attemptCtx, cancel = context.WithTimeout(ctx, retryCfg.PerAttemptTimeout)
			}
			value, err := attempt(attemptCtx, candidate)
			cancel()
			if err == nil {
				return value, candidate, attempts, nil
			}
			lastErr = err
			if !retryable(err, retryCfg) || n+1 == maxAttempts || (retryCfg.MaxElapsedTime > 0 && time.Since(started) >= retryCfg.MaxElapsedTime) {
				break
			}
			if err := sleepFn(ctx, backoffDuration(retryCfg, n, random)); err != nil {
				return zero, candidate, attempts, err
			}
		}
		if !failoverCfg.IsEnabled() {
			break
		}
	}
	return zero, lastCandidate, attempts, lastErr
}

func backoffDuration(cfg config.RetryConfig, retryIndex uint, random func() float64) time.Duration {
	if cfg.InitialInterval <= 0 {
		return 0
	}
	multiplier := cfg.Multiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	base := float64(cfg.InitialInterval) * math.Pow(multiplier, float64(retryIndex))
	if cfg.MaxInterval > 0 {
		base = math.Min(base, float64(cfg.MaxInterval))
	}
	if cfg.Jitter > 0 {
		base *= 1 + (2*random()-1)*cfg.Jitter
	}
	if cfg.MaxInterval > 0 {
		base = math.Min(base, float64(cfg.MaxInterval))
	}
	return time.Duration(math.Max(0, base))
}

func retryable(err error, cfg config.RetryConfig) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) {
		for _, status := range cfg.RetryableStatuses {
			if status == httpErr.StatusCode {
				return true
			}
		}
		return false
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
