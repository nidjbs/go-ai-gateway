package ratelimit

import (
	"testing"
	"time"

	"example.com/light-llm-gateway/internal/config"
)

func TestLimiterRegistryHasMemory(t *testing.T) {
	limiter, err := LimiterRegistry.Build("memory", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := limiter.(*memoryLimiter); !ok {
		t.Fatalf("memory driver returned %T; want *memoryLimiter", limiter)
	}
}

func TestQuotaRegistryHasMemory(t *testing.T) {
	store, err := QuotaRegistry.Build("memory", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*memoryQuotaStore); !ok {
		t.Fatalf("memory driver returned %T; want *memoryQuotaStore", store)
	}
}

func TestLimiterRegistryUnknownDriver(t *testing.T) {
	if _, err := LimiterRegistry.Build("redis", nil); err == nil {
		t.Fatal("expected error for unregistered driver")
	}
}

func TestLimiterFromRegistryAcceptsRequests(t *testing.T) {
	limiter, err := LimiterRegistry.Build("memory", nil)
	if err != nil {
		t.Fatal(err)
	}
	limits := config.KeyLimits{RPS: 5, Burst: 5}
	now := time.Now()
	decision := limiter.Allow("key-1", limits, now)
	if !decision.Allowed {
		t.Fatalf("first call allowed = false; want true")
	}
}
