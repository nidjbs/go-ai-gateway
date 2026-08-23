package ratelimit

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

// tokenBucketScript atomically refills a token bucket based on elapsed
// wall-clock time and consumes one token if available. A single round trip
// keeps the operation atomic across replicas.
//
// KEYS[1] = bucket hash key (e.g. "ratelimit:<keyID>")
// ARGV[1] = capacity (max tokens)
// ARGV[2] = refill rate (tokens per second)
// ARGV[3] = now_ms (current unix milliseconds, supplied by caller to keep
//
//	clock skew between replicas from racing the refill math)
//
// ARGV[4] = idle_ttl_seconds (EXPIRE on first write; long enough that a
//
//	quiescent bucket keeps its last refill timestamp)
//
// Returns { allowed (0|1), remaining_tokens (floor) }.
const tokenBucketScript = `
local key = KEYS[1]
local capacity = tonumber(ARGV[1])
local refill = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local data = redis.call('HMGET', key, 'tokens', 'last_refill_ms')
local tokens = tonumber(data[1])
local last_refill = tonumber(data[2])

if tokens == nil then
    tokens = capacity
    last_refill = now
end

local elapsed_ms = now - last_refill
if elapsed_ms > 0 then
    tokens = math.min(capacity, tokens + (elapsed_ms / 1000.0) * refill)
    last_refill = now
end

local allowed = 0
if tokens >= 1 then
    tokens = tokens - 1
    allowed = 1
end

redis.call('HMSET', key, 'tokens', tokens, 'last_refill_ms', last_refill)
if data[1] == false then
    redis.call('EXPIRE', key, ttl)
end

return { allowed, math.floor(tokens) }
`

// bucketIdleTTL is how long an untouched bucket sticks around in Redis.
const bucketIdleTTL = 3600

type redisLimiter struct {
	client redis.UniversalClient
	script *redis.Script
}

// NewRedisLimiter wraps an already-configured Redis client (standalone,
// Sentinel, or Cluster — any redis.Cmdable implementation works).
func NewRedisLimiter(client redis.UniversalClient) Limiter {
	return &redisLimiter{
		client: client,
		script: redis.NewScript(tokenBucketScript),
	}
}

// Allow implements Limiter. It fails open on Redis errors so a transient
// Redis outage does not amplify into a full request-storm rejection; the
// gateway logs the error elsewhere and operators can alert on the spike.
func (l *redisLimiter) Allow(keyID string, limits config.KeyLimits, now time.Time) Decision {
	if limits.RPS <= 0 {
		return Decision{Allowed: true, Limit: "unlimited", Remaining: -1}
	}
	capacity := float64(limits.Burst)
	if capacity <= 0 {
		capacity = limits.RPS
		if capacity < 1 {
			capacity = 1
		}
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := l.script.Run(ctx, l.client,
		[]string{"ratelimit:" + keyID},
		capacity, limits.RPS, now.UnixMilli(), bucketIdleTTL,
	).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return Decision{Allowed: true, Limit: formatLimit(limits), Remaining: -1}
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return Decision{Allowed: true, Limit: formatLimit(limits), Remaining: -1}
	}
	allowedRaw, _ := arr[0].(int64)
	remainingRaw, _ := arr[1].(int64)
	d := Decision{
		Allowed:   allowedRaw == 1,
		Limit:     formatLimit(limits),
		Remaining: remainingRaw,
	}
	if !d.Allowed && limits.RPS > 0 {
		d.RetryAfter = time.Duration(float64(time.Second) / limits.RPS)
	}
	return d
}
