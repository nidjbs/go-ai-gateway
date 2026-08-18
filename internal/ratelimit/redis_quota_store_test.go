package ratelimit

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// newTestRedisQuotaStore boots an in-process Redis and returns a
// redisQuotaStore pointed at it.
func newTestRedisQuotaStore(t *testing.T) (*redisQuotaStore, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	st := NewRedisQuotaStore(redisClientFor(s)).(*redisQuotaStore)
	return st, s
}

func TestRedisQuotaStoreChargeAccumulates(t *testing.T) {
	st, _ := newTestRedisQuotaStore(t)
	scope := QuotaScope{KeyID: "key-a", Window: WindowDaily}
	limit := int64(100)
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	s1 := st.Charge(scope, limit, 40, now)
	if s1.Used != 40 || s1.Remaining != 60 {
		t.Fatalf("first charge = %+v", s1)
	}
	s2 := st.Charge(scope, limit, 25, now)
	if s2.Used != 65 || s2.Remaining != 35 {
		t.Fatalf("second charge = %+v", s2)
	}
}

func TestRedisQuotaStorePeekReflectsPriorCharges(t *testing.T) {
	st, _ := newTestRedisQuotaStore(t)
	scope := QuotaScope{KeyID: "key-a", Window: WindowDaily}
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	st.Charge(scope, 100, 30, now)
	s := st.Peek(scope, 100, now)
	if s.Used != 30 || s.Remaining != 70 {
		t.Fatalf("peek = %+v, want used=30 remaining=70", s)
	}
}

func TestRedisQuotaStoreUnlimitedWhenLimitIsZero(t *testing.T) {
	st, _ := newTestRedisQuotaStore(t)
	now := time.Now().UTC()
	s := st.Charge(QuotaScope{KeyID: "key-a", Window: WindowDaily}, 0, 999, now)
	if s.Limit != 0 || s.Remaining != 0 {
		t.Fatalf("status = %+v, want unlimited semantics", s)
	}
}

func TestRedisQuotaStoreResetsAtUTCDayBoundary(t *testing.T) {
	st, s := newTestRedisQuotaStore(t)
	scope := QuotaScope{KeyID: "key-a", Window: WindowDaily}
	limit := int64(100)
	dayOne := time.Date(2026, 8, 11, 23, 59, 0, 0, time.UTC)
	dayTwo := time.Date(2026, 8, 12, 0, 1, 0, 0, time.UTC)
	st.Charge(scope, limit, 30, dayOne)
	// The bucket's TTL is computed from time-to-next-reset at charge time,
	// so by dayTwo it has expired. miniredis needs FastForward to advance
	// its internal clock and actually drop the key.
	s.FastForward(3 * time.Minute)
	s2 := st.Peek(scope, limit, dayTwo)
	if s2.Used != 0 || s2.Remaining != 100 {
		t.Fatalf("day two status = %+v", s2)
	}
}

func TestRedisQuotaStoreMonthlyWindow(t *testing.T) {
	st, ms := newTestRedisQuotaStore(t)
	scope := QuotaScope{KeyID: "key-a", Window: WindowMonthly}
	limit := int64(1000)
	aug := time.Date(2026, 8, 31, 23, 59, 0, 0, time.UTC)
	sep := time.Date(2026, 9, 1, 0, 1, 0, 0, time.UTC)
	st.Charge(scope, limit, 600, aug)
	// The aug bucket had a 60-second TTL; advance miniredis well past it
	// so the expiry is unambiguous.
	ms.FastForward(10 * time.Minute)
	status := st.Peek(scope, limit, sep)
	if status.Used != 0 {
		t.Fatalf("september peek used = %d; want 0 after window reset", status.Used)
	}
}

func TestRedisQuotaStoreAliasScope(t *testing.T) {
	st, _ := newTestRedisQuotaStore(t)
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	alias := QuotaScope{KeyID: "key-a", Alias: "chat", Window: WindowDaily}
	s := st.Charge(alias, 50, 30, now)
	if s.Used != 30 || s.Remaining != 20 {
		t.Fatalf("alias charge = %+v", s)
	}
}

func TestRedisQuotaStoreFailsOpenOnRedisDown(t *testing.T) {
	st, s := newTestRedisQuotaStore(t)
	s.Close()
	scope := QuotaScope{KeyID: "key-a", Window: WindowDaily}
	now := time.Now().UTC()
	// Peek returns a permissive status on error so we don't deny requests
	// based on partial data.
	d := st.Peek(scope, 100, now)
	if d.Remaining != 100 {
		t.Fatalf("peek remaining = %d, want 100 on fail-open", d.Remaining)
	}
}
