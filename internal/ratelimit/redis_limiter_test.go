package ratelimit

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"example.com/light-llm-gateway/internal/config"
)

// newTestRedisLimiter boots an in-process Redis and returns a redisLimiter
// pointed at it. miniredis implements the EVAL subset we need (HMGET,
// HMSET, INCRBY, SET, EXPIRE, EXISTS).
func newTestRedisLimiter(t *testing.T) (*redisLimiter, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	l := NewRedisLimiter(redisClientFor(s)).(*redisLimiter)
	return l, s
}

func TestRedisLimiterAllowsThenBlocks(t *testing.T) {
	l, _ := newTestRedisLimiter(t)
	limits := config.KeyLimits{RPS: 1, Burst: 1}
	now := time.Now().UTC()

	if !l.Allow("key-a", limits, now).Allowed {
		t.Fatal("first call must be allowed")
	}
	if l.Allow("key-a", limits, now).Allowed {
		t.Fatal("second call within the same instant must be denied")
	}
	// The bucket math keys off the `now` value supplied by Allow (so all
	// replicas see a consistent clock). Supplying a `now` 2 seconds later
	// refills the bucket at 1 token/sec.
	later := now.Add(2 * time.Second)
	if !l.Allow("key-a", limits, later).Allowed {
		t.Fatal("call after refill must be allowed")
	}
}

func TestRedisLimiterRPSZeroAlwaysAllows(t *testing.T) {
	l, _ := newTestRedisLimiter(t)
	limits := config.KeyLimits{RPS: 0}
	for i := 0; i < 5; i++ {
		if d := l.Allow("key-a", limits, time.Now()); !d.Allowed {
			t.Fatalf("call %d denied: %+v", i, d)
		}
	}
}

func TestRedisLimiterFailsOpenOnRedisDown(t *testing.T) {
	l, s := newTestRedisLimiter(t)
	s.Close()
	limits := config.KeyLimits{RPS: 1, Burst: 1}
	d := l.Allow("key-a", limits, time.Now())
	if !d.Allowed {
		t.Fatal("Limiter must fail open when Redis is unreachable")
	}
	if d.Remaining != -1 {
		t.Fatalf("Remaining = %d, want -1 on fail-open", d.Remaining)
	}
}

func TestRedisLimiterPerKeyIsolation(t *testing.T) {
	l, _ := newTestRedisLimiter(t)
	limits := config.KeyLimits{RPS: 1, Burst: 1}
	now := time.Now().UTC()
	if !l.Allow("alpha", limits, now).Allowed {
		t.Fatal("alpha first call must be allowed")
	}
	if !l.Allow("beta", limits, now).Allowed {
		t.Fatal("beta first call must be allowed (independent bucket)")
	}
	if l.Allow("alpha", limits, now).Allowed {
		t.Fatal("alpha second call must be denied")
	}
}
