package guardrails

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis driver registration. Registers itself with guardrails.TrackerRegistry
// when this package is imported (the gateway imports it transitively).
func init() {
	TrackerRegistry.Register("redis", newRedisTracker)
}

// trackerRecordScript atomically:
//  1. Returns 1 immediately if the key is already in penalty (so the
//     caller can report a "blocked" event without further work)
//  3. Otherwise increments the per-window counter (or seeds it to 1 with TTL)
//  2. Sets a penalty key with TTL when the counter hits max_attempts
//
// All in one round trip; replicas see identical state.
//
// KEYS[1] = count key     (guardrail:count:<keyID>)
// KEYS[2] = penalty key   (guardrail:penalty:<keyID>)
// ARGV[1] = window_seconds
// ARGV[2] = max_attempts
// ARGV[3] = penalty_seconds
//
// Returns 1 if the key should be reported as blocked, 0 otherwise.
const trackerRecordScript = `
if redis.call('EXISTS', KEYS[2]) == 1 then
    return 1
end
local exists = redis.call('EXISTS', KEYS[1])
local count
if exists == 0 then
    redis.call('SET', KEYS[1], 1, 'EX', tonumber(ARGV[1]))
    count = 1
else
    count = redis.call('INCR', KEYS[1])
end
if count >= tonumber(ARGV[2]) then
    redis.call('DEL', KEYS[1])
    redis.call('SET', KEYS[2], '1', 'EX', tonumber(ARGV[3]))
    return 1
end
return 0
`

type redisTracker struct {
	client *redis.Client
	script *redis.Script
	policy TrackerConfig
}

// newRedisTracker is the factory registered with TrackerRegistry. It pulls
// the policy back out of the merged opts map (placed there by
// server.pickGuardrailTracker) and reads backend options.
func newRedisTracker(opts map[string]any) (Tracker, error) {
	client, err := newRedisClientFromOpts(opts)
	if err != nil {
		return nil, err
	}
	policy := trackerPolicyFromOpts(opts)
	return &redisTracker{
		client: client,
		script: redis.NewScript(trackerRecordScript),
		policy: policy,
	}, nil
}

func newRedisClientFromOpts(opts map[string]any) (*redis.Client, error) {
	addr := stringOption(opts, "addr", "127.0.0.1:6379")
	if addr == "" {
		return nil, errors.New("redis tracker: addr is required")
	}
	rcfg := &redis.Options{
		Addr:         addr,
		Password:     stringOption(opts, "password", ""),
		DB:           intOption(opts, "db", 0),
		DialTimeout:  durationOption(opts, "dial_timeout", 2*time.Second),
		ReadTimeout:  durationOption(opts, "read_timeout", 250*time.Millisecond),
		WriteTimeout: durationOption(opts, "read_timeout", 250*time.Millisecond),
	}
	if boolOption(opts, "tls", false) {
		rcfg.TLSConfig = &tls.Config{}
	}
	client := redis.NewClient(rcfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis tracker ping: %w", err)
	}
	return client, nil
}

func (t *redisTracker) Record(keyID string, now time.Time) bool {
	if keyID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	windowSec := int64(t.policy.Window / time.Second)
	if windowSec <= 0 {
		windowSec = int64(t.policy.Window.Seconds())
	}
	penaltySec := int64(t.policy.Penalty / time.Second)
	if penaltySec <= 0 {
		penaltySec = int64(t.policy.Penalty.Seconds())
	}
	res, err := t.script.Run(ctx, t.client,
		[]string{"guardrail:count:" + keyID, "guardrail:penalty:" + keyID},
		windowSec, t.policy.MaxAttempts, penaltySec,
	).Int64()
	if err != nil {
		// Fail open: if Redis is down, we don't want to start blocking
		// legitimate traffic. Operators should alert on the error rate.
		return false
	}
	return res == 1
}

func (t *redisTracker) IsBlocked(keyID string, _ time.Time) bool {
	if keyID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := t.client.Exists(ctx, "guardrail:penalty:"+keyID).Result()
	if err != nil {
		return false
	}
	return res == 1
}

func (t *redisTracker) PenaltyRemaining(keyID string, now time.Time) time.Duration {
	if keyID == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ttl, err := t.client.TTL(ctx, "guardrail:penalty:"+keyID).Result()
	if err != nil {
		return 0
	}
	if ttl < 0 {
		return 0
	}
	return ttl
}

func (t *redisTracker) Reset(keyID string) {
	if keyID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = t.client.Del(ctx,
		"guardrail:count:"+keyID,
		"guardrail:penalty:"+keyID,
	).Err()
}

func (t *redisTracker) ActiveBlocks(_ time.Time) int {
	// SCAN is the safe option here; KEYS is O(N) and would block Redis on
	// large keyspaces. ActiveBlocks is a monitoring call, not a hot path,
	// so the extra round trip is acceptable.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var (
		cursor uint64
		count  int
	)
	for {
		keys, next, err := t.client.Scan(ctx, cursor, "guardrail:penalty:*", 100).Result()
		if err != nil {
			return count
		}
		count += len(keys)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return count
}

// Compile-time guarantee that the Redis tracker satisfies Tracker.
var _ Tracker = (*redisTracker)(nil)
