package ratelimit

import "example.com/light-llm-gateway/internal/registry"

// LimiterRegistry and QuotaRegistry hold the driver factories the gateway uses
// to build Limiter and QuotaStore instances from configuration. The default
// "memory" drivers are registered in init(); third-party packages can register
// additional drivers (e.g. "redis") in their own init() and the binary picks
// them up via the config driver name.
var (
	LimiterRegistry = registry.NewRegistry[Limiter]()
	QuotaRegistry   = registry.NewRegistry[QuotaStore]()
)

func init() {
	LimiterRegistry.Register("memory", func(_ map[string]any) (Limiter, error) { return NewMemoryLimiter(), nil })
	QuotaRegistry.Register("memory", func(_ map[string]any) (QuotaStore, error) { return NewMemoryQuotaStore(), nil })
}
