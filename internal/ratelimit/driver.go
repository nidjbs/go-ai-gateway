package ratelimit

import (
	"example.com/light-llm-gateway/internal/registry"
)

// LimiterRegistry and QuotaRegistry hold driver factories. Default "memory"
// and "redis" drivers are registered in init(); third-party packages can
// register additional drivers via their own init().
var (
	LimiterRegistry = registry.NewRegistry[Limiter]()
	QuotaRegistry   = registry.NewRegistry[QuotaStore]()
)

func init() {
	LimiterRegistry.Register("memory", func(_ map[string]any) (Limiter, error) {
		return NewMemoryLimiter(), nil
	})
	QuotaRegistry.Register("memory", func(_ map[string]any) (QuotaStore, error) {
		return NewMemoryQuotaStore(), nil
	})
	LimiterRegistry.Register("redis", newRedisLimiter)
	QuotaRegistry.Register("redis", newRedisQuotaStore)
}
