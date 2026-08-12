package ratelimit

import (
	"time"

	"example.com/light-llm-gateway/internal/config"
)

// Decision describes whether a single request is allowed by the rate limiter.
type Decision struct {
	Allowed    bool
	Limit      string
	Remaining  int64
	RetryAfter time.Duration
}

// Limiter is the rate-limit interface used by the gateway.
//
// Implementations must be safe for concurrent use.
type Limiter interface {
	// Allow attempts to take one token from the limiter bucket associated
	// with keyID. When limits.RPS is zero the limiter must allow all calls.
	Allow(keyID string, limits config.KeyLimits, now time.Time) Decision
}

// QuotaStatus describes the daily token usage for a single api key.
type QuotaStatus struct {
	Limit     int64
	Used      int64
	Remaining int64
	ResetAt   time.Time
}

// QuotaStore tracks per-key token usage for the current UTC day.
//
// Peek returns the current status without consuming tokens. Charge adds
// delta tokens and returns the resulting status.
//
// Implementations must be safe for concurrent use.
type QuotaStore interface {
	Peek(keyID string, limit int64, now time.Time) QuotaStatus
	Charge(keyID string, limit int64, delta int64, now time.Time) QuotaStatus
}

// UnlimitedQuotaStatus is returned when no per-day token limit is configured.
func UnlimitedQuotaStatus(now time.Time) QuotaStatus {
	return QuotaStatus{
		Limit:     0,
		Used:      0,
		Remaining: 0,
		ResetAt:   nextUTCMidnight(now),
	}
}

func nextUTCMidnight(now time.Time) time.Time {
	t := now.UTC()
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
}
