package retry

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/provider"
	"example.com/light-llm-gateway/internal/routing"
)

type Attempts struct{ Total int }

func Execute[T any](ctx context.Context, retryCfg config.RetryConfig, failoverCfg config.FailoverConfig, candidates []routing.Candidate, attempt func(context.Context, routing.Candidate) (T, error)) (T, routing.Candidate, Attempts, error) {
	var zero T
	var lastCandidate routing.Candidate
	var lastErr error
	attempts := Attempts{}
	limit := len(candidates)
	if failoverCfg.MaxProviders > 0 && int(failoverCfg.MaxProviders) < limit {
		limit = int(failoverCfg.MaxProviders)
	}
	for i := 0; i < limit; i++ {
		candidate := candidates[i]
		lastCandidate = candidate
		started := time.Now()
		for n := uint(0); n < retryCfg.MaxAttemptsPerProvider; n++ {
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
			if !retryable(err, retryCfg) || n+1 == retryCfg.MaxAttemptsPerProvider || time.Since(started) >= retryCfg.MaxElapsedTime {
				break
			}
			if err := sleep(ctx, retryCfg.InitialInterval); err != nil {
				return zero, candidate, attempts, err
			}
		}
		if !failoverCfg.Enabled {
			break
		}
	}
	return zero, lastCandidate, attempts, lastErr
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
