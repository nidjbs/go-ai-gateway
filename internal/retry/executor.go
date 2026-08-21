package retry

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"slices"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"example.com/light-llm-gateway/internal/circuitbreaker"
	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/provider"
	"example.com/light-llm-gateway/internal/routing"
)

type Attempts struct {
	Total     int
	Retries   int
	Failovers int
}

// Execute runs request against the candidate list with per-provider retries and provider-level failover. breaker may be nil (treated as Noop).
func Execute[T any](ctx context.Context, retryCfg config.RetryConfig, failoverCfg config.FailoverConfig, breaker circuitbreaker.Breaker, candidates []routing.Candidate, attempt func(context.Context, routing.Candidate) (T, error)) (T, routing.Candidate, Attempts, error) {
	if breaker == nil {
		breaker = circuitbreaker.Noop{}
	}
	return execute(ctx, retryCfg, failoverCfg, breaker, candidates, attempt, sleep, rand.Float64)
}

func execute[T any](ctx context.Context, retryCfg config.RetryConfig, failoverCfg config.FailoverConfig, breaker circuitbreaker.Breaker, candidates []routing.Candidate, attempt func(context.Context, routing.Candidate) (T, error), sleepFn func(context.Context, time.Duration) error, random func() float64) (T, routing.Candidate, Attempts, error) {
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
	now := time.Now()
	for i := 0; i < limit; i++ {
		if i > 0 {
			attempts.Failovers++
		}
		candidate := candidates[i]
		lastCandidate = candidate
		breakerKey := candidate.Name + "|" + candidate.BaseURL

		// Fast path: skip candidates whose breaker is already open.
		if err := breaker.Allow(breakerKey, now); err != nil {
			lastErr = err
			continue
		}

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
			attemptCtx, attemptSpan := otel.Tracer("gateway.retry").Start(attemptCtx, "upstream.attempt",
				trace.WithAttributes(
					attribute.String("provider", candidate.Name),
					attribute.String("model", candidate.Model),
					attribute.String("base_url", candidate.BaseURL),
					attribute.Int("attempt_index", int(n)+1),
				),
			)
			value, err := attempt(attemptCtx, candidate)
			if err != nil {
				attemptSpan.RecordError(err)
				attemptSpan.SetStatus(codes.Error, err.Error())
			}
			attemptSpan.End()
			breaker.Record(breakerKey, now, err)
			if err != nil {
				cancel()
				lastErr = err
				// If the breaker just tripped, abandon remaining attempts for this candidate.
				if isOpenErr := breaker.Allow(breakerKey, now); isOpenErr != nil {
					break
				}
				if !retryable(err, retryCfg) || n+1 == maxAttempts || (retryCfg.MaxElapsedTime > 0 && time.Since(started) >= retryCfg.MaxElapsedTime) {
					break
				}
				if err := sleepFn(ctx, backoffDuration(retryCfg, n, random)); err != nil {
					return zero, candidate, attempts, err
				}
				continue
			}
			// On success, ownership of attemptCtx transfers to the returned value (streams rely on it; the value's Close reclaims it).
			cancel()
			return value, candidate, attempts, nil
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
	// ProviderError is the canonical signal: protocol/invalid_request/canceled
	// are never retryable; network/timeout retry by default; upstream retry
	// only when its status is in the configured retryable set.
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		switch pe.Kind {
		case provider.ErrorKindProtocol, provider.ErrorKindInvalidRequest, provider.ErrorKindCanceled:
			return false
		case provider.ErrorKindNetwork, provider.ErrorKindTimeout:
			return true
		case provider.ErrorKindUpstream:
			return slices.Contains(cfg.RetryableStatuses, pe.Status)
		case provider.ErrorKindUnknown:
			// fall through to legacy detection below.
		}
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) {
		return slices.Contains(cfg.RetryableStatuses, httpErr.StatusCode)
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
