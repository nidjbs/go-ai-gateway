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

// QuotaWindow identifies the time window over which a quota is tracked.
type QuotaWindow int

const (
	// WindowDaily resets at the next UTC midnight.
	WindowDaily QuotaWindow = iota
	// WindowMonthly resets at the first instant of the next UTC month.
	WindowMonthly
)

// QuotaScope identifies which bucket of a QuotaStore a call targets.
//
// Alias is empty for key-aggregate quotas (daily/monthly totals across all
// aliases the key has hit). When Alias is non-empty the bucket counts only
// tokens used against that alias.
type QuotaScope struct {
	KeyID  string
	Alias  string
	Window QuotaWindow
}

// QuotaStatus describes the token usage for a single (KeyID, Alias, Window)
// bucket.
type QuotaStatus struct {
	Window    QuotaWindow
	Limit     int64
	Used      int64
	Remaining int64
	ResetAt   time.Time
}

// QuotaStore tracks per-key and per-(key, alias) token usage across multiple
// time windows.
//
// Peek returns the current status without consuming tokens. Charge adds
// delta tokens and returns the resulting status.
//
// Implementations must be safe for concurrent use.
type QuotaStore interface {
	Peek(scope QuotaScope, limit int64, now time.Time) QuotaStatus
	Charge(scope QuotaScope, limit int64, delta int64, now time.Time) QuotaStatus
}

// UnlimitedQuotaStatus is returned when no quota limit is configured for the
// given window.
func UnlimitedQuotaStatus(window QuotaWindow, now time.Time) QuotaStatus {
	return QuotaStatus{
		Window:    window,
		Limit:     0,
		Used:      0,
		Remaining: 0,
		ResetAt:   nextReset(window, now),
	}
}

func nextReset(window QuotaWindow, now time.Time) time.Time {
	switch window {
	case WindowMonthly:
		return nextUTCMonthStart(now)
	default:
		return nextUTCMidnight(now)
	}
}

func nextUTCMidnight(now time.Time) time.Time {
	t := now.UTC()
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
}

func nextUTCMonthStart(now time.Time) time.Time {
	t := now.UTC()
	return time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}
