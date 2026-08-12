package ratelimit

import (
	"strconv"
	"testing"
	"time"

	"example.com/light-llm-gateway/internal/config"
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

	used := store.Charge("key-a", limit, 30, dayOne)
	if used.Used != 30 || used.Remaining != 70 {
		t.Fatalf("day one status = %+v", used)
	}

	reset := store.Peek("key-a", limit, dayTwo)
	if reset.Used != 0 || reset.Remaining != 100 {
		t.Fatalf("day two status = %+v", reset)
	}
}

func TestMemoryQuotaStoreUnlimitedWhenLimitIsZero(t *testing.T) {
	store := NewMemoryQuotaStore()
	status := store.Charge("key-a", 0, 999, time.Now().UTC())
	if status.Limit != 0 || status.Remaining != 0 {
		t.Fatalf("status = %+v, want unlimited semantics", status)
	}
}

func TestMemoryQuotaStoreChargeAccumulates(t *testing.T) {
	store := NewMemoryQuotaStore()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if status := store.Charge("key-a", 100, 40, now); status.Used != 40 {
		t.Fatalf("first charge = %+v", status)
	}
	if status := store.Charge("key-a", 100, 25, now); status.Used != 65 || status.Remaining != 35 {
		t.Fatalf("second charge = %+v", status)
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
