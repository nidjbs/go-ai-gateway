package ratelimit

import (
	"sync"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

// NewMemoryLimiter returns an in-process token-bucket based Limiter.
func NewMemoryLimiter() Limiter {
	return &memoryLimiter{buckets: make(map[string]*tokenBucket)}
}

type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillPerS float64
	lastRefill time.Time
}

type memoryLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

// Allow reserves one token for keyID. When limits.RPS is zero or negative
// every call is allowed without consuming a token.
func (l *memoryLimiter) Allow(keyID string, limits config.KeyLimits, now time.Time) Decision {
	if limits.RPS <= 0 {
		return Decision{Allowed: true, Limit: "unlimited", Remaining: -1}
	}
	if keyID == "" {
		return Decision{Allowed: true, Limit: formatLimit(limits), Remaining: -1}
	}
	bucket := l.bucketFor(keyID, limits)
	return bucket.consume(now, limits)
}

func (l *memoryLimiter) bucketFor(keyID string, limits config.KeyLimits) *tokenBucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, ok := l.buckets[keyID]
	if !ok {
		burst := float64(limits.Burst)
		if burst <= 0 {
			burst = limits.RPS
			if burst < 1 {
				burst = 1
			}
		}
		bucket = &tokenBucket{
			tokens:     burst,
			maxTokens:  burst,
			refillPerS: limits.RPS,
			lastRefill: time.Now().UTC(),
		}
		l.buckets[keyID] = bucket
	}
	return bucket
}

func (b *tokenBucket) consume(now time.Time, limits config.KeyLimits) Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if now.After(b.lastRefill) {
		elapsed := now.Sub(b.lastRefill).Seconds()
		b.tokens += elapsed * b.refillPerS
		if b.tokens > b.maxTokens {
			b.tokens = b.maxTokens
		}
		b.lastRefill = now
	}
	if b.tokens >= 1 {
		b.tokens -= 1
		remaining := int64(b.tokens)
		return Decision{Allowed: true, Limit: formatLimit(limits), Remaining: remaining}
	}
	missing := 1 - b.tokens
	wait := time.Duration(missing / b.refillPerS * float64(time.Second))
	return Decision{Allowed: false, Limit: formatLimit(limits), Remaining: 0, RetryAfter: wait}
}

func formatLimit(limits config.KeyLimits) string {
	burst := limits.Burst
	if burst <= 0 {
		burst = int(limits.RPS)
		if burst < 1 {
			burst = 1
		}
	}
	return formatFloat(limits.RPS) + " rps, burst " + itoa(burst)
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return itoa64(int64(v))
	}
	return strconvFloat(v)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 8)
	if n == 0 {
		return "0"
	}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 8)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

func strconvFloat(v float64) string {
	const digits = "0123456789"
	if v < 0 {
		return "-" + strconvFloat(-v)
	}
	intPart := int64(v)
	frac := v - float64(intPart)
	const precision = 6
	out := itoa64(intPart)
	if frac == 0 {
		return out
	}
	out += "."
	for i := 0; i < precision; i++ {
		frac *= 10
		d := int64(frac)
		out += string(digits[d])
		frac -= float64(d)
		if frac == 0 {
			break
		}
	}
	return out
}

// NewMemoryQuotaStore returns an in-process QuotaStore backed by a map.
func NewMemoryQuotaStore() QuotaStore {
	return &memoryQuotaStore{values: make(map[string]quotaValue)}
}

type memoryQuotaStore struct {
	mu     sync.Mutex
	values map[string]quotaValue
}

type quotaValue struct {
	windowStart time.Time
	used        int64
}

func (s *memoryQuotaStore) Peek(scope QuotaScope, limit int64, now time.Time) QuotaStatus {
	if limit <= 0 {
		return UnlimitedQuotaStatus(scope.Window, now)
	}
	if scope.KeyID == "" {
		return UnlimitedQuotaStatus(scope.Window, now)
	}
	bucketStart := windowStart(scope.Window, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[bucketKey(scope)]
	if !ok || !value.windowStart.Equal(bucketStart) {
		return QuotaStatus{Window: scope.Window, Limit: limit, Used: 0, Remaining: limit, ResetAt: nextReset(scope.Window, now)}
	}
	remaining := limit - value.used
	if remaining < 0 {
		remaining = 0
	}
	return QuotaStatus{Window: scope.Window, Limit: limit, Used: value.used, Remaining: remaining, ResetAt: nextReset(scope.Window, now)}
}

func (s *memoryQuotaStore) Charge(scope QuotaScope, limit int64, delta int64, now time.Time) QuotaStatus {
	if limit <= 0 || delta <= 0 || scope.KeyID == "" {
		return s.Peek(scope, limit, now)
	}
	bucketStart := windowStart(scope.Window, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bucketKey(scope)
	value, ok := s.values[key]
	if !ok || !value.windowStart.Equal(bucketStart) {
		value = quotaValue{windowStart: bucketStart}
	}
	value.used += delta
	s.values[key] = value
	status := quotaStatusFor(scope, limit, value.used, now)
	// Alias charges also update the key-aggregate bucket to prevent quota drift.
	if scope.Alias != "" {
		aggregate := QuotaScope{KeyID: scope.KeyID, Window: scope.Window}
		aggKey := bucketKey(aggregate)
		aggValue, aggOK := s.values[aggKey]
		if !aggOK || !aggValue.windowStart.Equal(bucketStart) {
			aggValue = quotaValue{windowStart: bucketStart}
		}
		aggValue.used += delta
		s.values[aggKey] = aggValue
	}
	return status
}

func quotaStatusFor(scope QuotaScope, limit int64, used int64, now time.Time) QuotaStatus {
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return QuotaStatus{Window: scope.Window, Limit: limit, Used: used, Remaining: remaining, ResetAt: nextReset(scope.Window, now)}
}

func bucketKey(scope QuotaScope) string {
	return strconvItoa(int(scope.Window)) + "|" + scope.KeyID + "|" + scope.Alias
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 8)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

func windowStart(window QuotaWindow, now time.Time) time.Time {
	t := now.UTC()
	switch window {
	case WindowMonthly:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
}
