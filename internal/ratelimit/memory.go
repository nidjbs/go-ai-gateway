package ratelimit

import (
	"sync"
	"time"

	"example.com/light-llm-gateway/internal/config"
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
	dayStart time.Time
	used     int64
}

func (s *memoryQuotaStore) Peek(keyID string, limit int64, now time.Time) QuotaStatus {
	if limit <= 0 {
		return UnlimitedQuotaStatus(now)
	}
	day := utcDayStart(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[keyID]
	if !ok || !value.dayStart.Equal(day) {
		return QuotaStatus{Limit: limit, Used: 0, Remaining: limit, ResetAt: nextUTCMidnight(now)}
	}
	remaining := limit - value.used
	if remaining < 0 {
		remaining = 0
	}
	return QuotaStatus{Limit: limit, Used: value.used, Remaining: remaining, ResetAt: nextUTCMidnight(now)}
}

func (s *memoryQuotaStore) Charge(keyID string, limit int64, delta int64, now time.Time) QuotaStatus {
	if limit <= 0 || delta <= 0 || keyID == "" {
		return s.Peek(keyID, limit, now)
	}
	day := utcDayStart(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[keyID]
	if !ok || !value.dayStart.Equal(day) {
		value = quotaValue{dayStart: day}
	}
	value.used += delta
	s.values[keyID] = value
	remaining := limit - value.used
	if remaining < 0 {
		remaining = 0
	}
	return QuotaStatus{Limit: limit, Used: value.used, Remaining: remaining, ResetAt: nextUTCMidnight(now)}
}

func utcDayStart(now time.Time) time.Time {
	t := now.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
