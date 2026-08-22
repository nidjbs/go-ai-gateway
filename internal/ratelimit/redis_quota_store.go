package ratelimit

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// quotaChargeScript atomically increments a counter under a fixed-window
// TTL aligned to the next UTC midnight (daily) or month-start (monthly).
// One round trip keeps every replica seeing the same bucket even when many
// are racing to charge the same key.
//
// KEYS[1] = bucket key (e.g. "quota:daily:keyID" or "quota:daily:keyID:alias")
// ARGV[1] = limit (returned in remaining)
// ARGV[2] = delta (tokens to add)
// ARGV[3] = ttl_seconds (until next reset)
//
// Returns { used, remaining }.
const quotaChargeScript = `
local limit = tonumber(ARGV[1])
local delta = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local exists = redis.call('EXISTS', KEYS[1])
local used
if exists == 0 then
    used = delta
    redis.call('SET', KEYS[1], used, 'EX', ttl)
else
    used = redis.call('INCRBY', KEYS[1], delta)
    -- refresh TTL on every charge so an actively-used bucket never expires
    -- mid-window. Without this, a single INCRBY just before expiry would
    -- outlive its window.
    redis.call('EXPIRE', KEYS[1], ttl)
end
local remaining = limit - used
if remaining < 0 then remaining = 0 end
return { used, remaining }
`

type redisQuotaStore struct {
	client *redis.Client
	script *redis.Script
}

// NewRedisQuotaStore wraps an already-configured *redis.Client.
func NewRedisQuotaStore(client *redis.Client) QuotaStore {
	return &redisQuotaStore{
		client: client,
		script: redis.NewScript(quotaChargeScript),
	}
}

func (s *redisQuotaStore) Peek(scope QuotaScope, limit int64, now time.Time) QuotaStatus {
	if limit <= 0 || scope.KeyID == "" {
		return UnlimitedQuotaStatus(scope.Window, now)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := s.client.Get(ctx, quotaKey(scope)).Result()
	used := int64(0)
	if err == nil {
		if v, parseErr := strconv.ParseInt(res, 10, 64); parseErr == nil {
			used = v
		}
	} else if !errors.Is(err, redis.Nil) {
		// On error we still return a valid status with the limit; the
		// gateway should fail open at the quota layer too. Remaining = limit
		// is the conservative choice — refuse to deny based on partial data.
		return QuotaStatus{
			Window:    scope.Window,
			Limit:     limit,
			Used:      0,
			Remaining: limit,
			ResetAt:   nextReset(scope.Window, now),
		}
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return QuotaStatus{
		Window:    scope.Window,
		Limit:     limit,
		Used:      used,
		Remaining: remaining,
		ResetAt:   nextReset(scope.Window, now),
	}
}

func (s *redisQuotaStore) Charge(scope QuotaScope, limit int64, delta int64, now time.Time) QuotaStatus {
	if limit <= 0 || delta <= 0 || scope.KeyID == "" {
		return s.Peek(scope, limit, now)
	}
	resetAt := nextReset(scope.Window, now)
	// TTL is the time from `now` (the charge instant) to the next reset
	// boundary. We use resetAt-now rather than time.Until(resetAt) so the
	// bucket always expires at the natural boundary even when callers pass
	// in a `now` that is far from real time (tests, replay tooling, etc.).
	ttl := resetAt.Sub(now)
	if ttl <= 0 {
		// Edge case: now is exactly at the boundary. Use a 1s TTL so the
		// next request lands in the new window.
		ttl = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := s.script.Run(ctx, s.client,
		[]string{quotaKey(scope)},
		limit, delta, int64(ttl.Seconds()),
	).Result()
	if err != nil {
		// Fail-open: return current state via Peek.
		return s.Peek(scope, limit, now)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return s.Peek(scope, limit, now)
	}
	usedRaw, _ := arr[0].(int64)
	remainingRaw, _ := arr[1].(int64)
	if remainingRaw < 0 {
		remainingRaw = 0
	}
	return QuotaStatus{
		Window:    scope.Window,
		Limit:     limit,
		Used:      usedRaw,
		Remaining: remainingRaw,
		ResetAt:   resetAt,
	}
}

// quotaKey is the on-the-wire key layout. Including the Window value
// (0=daily, 1=monthly) lets us evolve the format later without aliasing
// keys across windows.
func quotaKey(scope QuotaScope) string {
	windowName := "daily"
	if scope.Window == WindowMonthly {
		windowName = "monthly"
	}
	metric := scope.Metric
	if metric == "" {
		metric = "tokens"
	}
	key := "quota:" + windowName + ":" + metric + ":" + scope.KeyID
	if scope.Alias != "" {
		key += ":" + scope.Alias
	}
	return key
}
