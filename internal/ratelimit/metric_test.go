package ratelimit

import (
	"testing"
	"time"
)

// TestQuotaMetricDimensionsAreSeparate verifies that request counters and
// token buckets sharing a key never alias, even though both are daily-window
// counters under the same KeyID.
func TestQuotaMetricDimensionsAreSeparate(t *testing.T) {
	store := NewMemoryQuotaStore()
	now := time.Now()

	tokens := QuotaScope{KeyID: "k1", Window: WindowDaily}
	requests := QuotaScope{KeyID: "k1", Window: WindowDaily, Metric: RequestsMetric}

	store.Charge(tokens, 1000, 500, now)
	store.Charge(requests, 10, 3, now)

	if got := store.Peek(tokens, 1000, now).Used; got != 500 {
		t.Fatalf("token bucket used = %d, want 500", got)
	}
	if got := store.Peek(requests, 10, now).Used; got != 3 {
		t.Fatalf("request counter used = %d, want 3", got)
	}
}

func TestQuotaKeyIncludesMetric(t *testing.T) {
	if got := quotaKey(QuotaScope{KeyID: "k", Window: WindowDaily}); got != "quota:daily:tokens:k" {
		t.Fatalf("token key = %q", got)
	}
	if got := quotaKey(QuotaScope{KeyID: "k", Window: WindowDaily, Metric: RequestsMetric}); got != "quota:daily:requests:k" {
		t.Fatalf("request key = %q", got)
	}
	if got := quotaKey(QuotaScope{KeyID: "k", Alias: "chat", Window: WindowDaily}); got != "quota:daily:tokens:k:chat" {
		t.Fatalf("alias token key = %q", got)
	}
}
