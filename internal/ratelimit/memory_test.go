package ratelimit

import (
	"strconv"
	"testing"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

func TestMemoryLimiterRejectsWhenBurstIsExceeded(t *testing.T) {
	limiter := NewMemoryLimiter()
	limits := config.KeyLimits{RPS: 1, Burst: 1}
	now := time.Now().UTC()

	first := limiter.Allow("key-a", limits, now)
	second := limiter.Allow("key-a", limits, now)
	if !first.Allowed {
		t.Fatalf("first decision = %+v, want allowed", first)
	}
	if second.Allowed {
		t.Fatalf("second decision = %+v, want denied", second)
	}
	if second.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want > 0", second.RetryAfter)
	}
}

func TestMemoryLimiterAllowsWhenRPSIsZero(t *testing.T) {
	limiter := NewMemoryLimiter()
	limits := config.KeyLimits{RPS: 0, Burst: 0}
	for i := 0; i < 5; i++ {
		if decision := limiter.Allow("key-a", limits, time.Now()); !decision.Allowed {
			t.Fatalf("decision = %+v, want allowed", decision)
		}
	}
}

func TestMemoryLimiterRefillsAfterDelay(t *testing.T) {
	limiter := NewMemoryLimiter()
	limits := config.KeyLimits{RPS: 1, Burst: 1}
	start := time.Now().UTC()

	if !limiter.Allow("key-a", limits, start).Allowed {
		t.Fatal("first call must be allowed")
	}
	if limiter.Allow("key-a", limits, start).Allowed {
		t.Fatal("second call within the same instant must be denied")
	}
	later := start.Add(2 * time.Second)
	if !limiter.Allow("key-a", limits, later).Allowed {
		t.Fatal("call after refill must be allowed")
	}
}

func TestMemoryQuotaStoreResetsAtUTCDayBoundary(t *testing.T) {
	store := NewMemoryQuotaStore()
	limit := int64(100)
	dayOne := time.Date(2026, 8, 11, 23, 59, 0, 0, time.UTC)
	dayTwo := time.Date(2026, 8, 12, 0, 1, 0, 0, time.UTC)

	scope := QuotaScope{KeyID: "key-a", Window: WindowDaily}
	used := store.Charge(scope, limit, 30, dayOne)
	if used.Used != 30 || used.Remaining != 70 {
		t.Fatalf("day one status = %+v", used)
	}

	reset := store.Peek(scope, limit, dayTwo)
	if reset.Used != 0 || reset.Remaining != 100 {
		t.Fatalf("day two status = %+v", reset)
	}
}

func TestMemoryQuotaStoreUnlimitedWhenLimitIsZero(t *testing.T) {
	store := NewMemoryQuotaStore()
	status := store.Charge(QuotaScope{KeyID: "key-a", Window: WindowDaily}, 0, 999, time.Now().UTC())
	if status.Limit != 0 || status.Remaining != 0 {
		t.Fatalf("status = %+v, want unlimited semantics", status)
	}
}

func TestMemoryQuotaStoreChargeAccumulates(t *testing.T) {
	store := NewMemoryQuotaStore()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	scope := QuotaScope{KeyID: "key-a", Window: WindowDaily}
	if status := store.Charge(scope, 100, 40, now); status.Used != 40 {
		t.Fatalf("first charge = %+v", status)
	}
	if status := store.Charge(scope, 100, 25, now); status.Used != 65 || status.Remaining != 35 {
		t.Fatalf("second charge = %+v", status)
	}
}

func TestMemoryQuotaStoreMonthlyResetsAtMonthBoundary(t *testing.T) {
	store := NewMemoryQuotaStore()
	scope := QuotaScope{KeyID: "key-a", Window: WindowMonthly}
	limit := int64(1000)
	aug := time.Date(2026, 8, 31, 23, 59, 0, 0, time.UTC)
	sep := time.Date(2026, 9, 1, 0, 1, 0, 0, time.UTC)

	if status := store.Charge(scope, limit, 600, aug); status.Used != 600 {
		t.Fatalf("august charge = %+v", status)
	}
	status := store.Peek(scope, limit, sep)
	if status.Used != 0 || status.Remaining != 1000 {
		t.Fatalf("september peek = %+v", status)
	}
}

func TestMemoryQuotaStoreAliasScopeIsolatedFromKeyAggregate(t *testing.T) {
	store := NewMemoryQuotaStore()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	keyScope := QuotaScope{KeyID: "key-a", Window: WindowDaily}
	aliasScope := QuotaScope{KeyID: "key-a", Alias: "chat", Window: WindowDaily}

	store.Charge(keyScope, 1000, 100, now)
	if status := store.Charge(aliasScope, 50, 30, now); status.Used != 30 {
		t.Fatalf("alias charge = %+v", status)
	}
	if status := store.Peek(keyScope, 1000, now); status.Used != 130 {
		t.Fatalf("key aggregate = %+v", status)
	}
	if status := store.Peek(aliasScope, 50, now); status.Used != 30 {
		t.Fatalf("alias peek = %+v", status)
	}
}

func TestFormatLimitUsesBurst(t *testing.T) {
	got := formatLimit(config.KeyLimits{RPS: 4, Burst: 9})
	want := strconv.FormatFloat(4, 'f', -1, 64) + " rps, burst 9"
	if got != want {
		t.Fatalf("formatLimit = %q, want %q", got, want)
	}
}

func TestItoa(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{-7, "-7"},
	}
	for _, tc := range cases {
		if got := itoa(tc.in); got != tc.want {
			t.Fatalf("itoa(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
