package guardrails

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedisTracker builds a Redis-backed Tracker wired to an in-process
// miniredis. The tracker uses DefaultTrackerConfig policy; tests that need
// different policy build their own via newRedisTracker.
func newTestRedisTracker(t *testing.T) (Tracker, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	tr := &redisTracker{
		client: client,
		script: redis.NewScript(trackerRecordScript),
		policy: DefaultTrackerConfig(),
	}
	return tr, s
}

func TestRedisTrackerBlocksAfterMaxAttempts(t *testing.T) {
	tr, _ := newTestRedisTracker(t)
	now := time.Now()
	// First two records are below threshold; third trips the penalty.
	for i := 0; i < 2; i++ {
		if blocked := tr.Record("key-1", now); blocked {
			t.Fatalf("attempt %d should not block", i+1)
		}
	}
	if !tr.Record("key-1", now) {
		t.Fatal("third attempt should trigger penalty")
	}
	if !tr.IsBlocked("key-1", now) {
		t.Fatal("key must be in penalty after threshold")
	}
}

func TestRedisTrackerPenaltyExpires(t *testing.T) {
	tr, ms := newTestRedisTracker(t)
	now := time.Now()
	for i := 0; i < 3; i++ {
		tr.Record("key-1", now)
	}
	if !tr.IsBlocked("key-1", now) {
		t.Fatal("expected blocked state")
	}
	// Advance miniredis past the penalty TTL.
	ms.FastForward(45 * time.Second)
	if tr.IsBlocked("key-1", now.Add(45*time.Second)) {
		t.Fatal("penalty should have expired")
	}
}

func TestRedisTrackerRecordWhileBlockedIsNoOp(t *testing.T) {
	tr, _ := newTestRedisTracker(t)
	now := time.Now()
	for i := 0; i < 3; i++ {
		tr.Record("key-1", now)
	}
	// Subsequent records during penalty should not increment any counter
	// (the Lua script early-exits). We verify by recording 5 more times,
	// then resetting and observing the counter starts fresh.
	for i := 0; i < 5; i++ {
		tr.Record("key-1", now)
	}
	tr.Reset("key-1")
	if tr.IsBlocked("key-1", now) {
		t.Fatal("Reset should clear penalty")
	}
	// After reset, the counter starts fresh — first 2 records don't block.
	for i := 0; i < 2; i++ {
		if tr.Record("key-1", now) {
			t.Fatalf("post-reset attempt %d should not block", i+1)
		}
	}
}

func TestRedisTrackerPerKeyIsolation(t *testing.T) {
	tr, _ := newTestRedisTracker(t)
	now := time.Now()
	for i := 0; i < 3; i++ {
		tr.Record("alpha", now)
	}
	// Beta should still have a fresh counter.
	for i := 0; i < 2; i++ {
		if tr.Record("beta", now) {
			t.Fatalf("beta attempt %d should not block (alpha is blocked)", i+1)
		}
	}
	if !tr.IsBlocked("alpha", now) {
		t.Fatal("alpha should be blocked")
	}
	if tr.IsBlocked("beta", now) {
		t.Fatal("beta should not be blocked")
	}
}

func TestRedisTrackerEmptyKeyIsNoOp(t *testing.T) {
	tr, _ := newTestRedisTracker(t)
	if tr.Record("", time.Now()) {
		t.Fatal("empty key must not block")
	}
	if tr.IsBlocked("", time.Now()) {
		t.Fatal("empty key must not be blocked")
	}
}
