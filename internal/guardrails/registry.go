package guardrails

import (
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/registry"
)

// TrackerRegistry maps configured driver names to Tracker factories. The
// "memory" driver is registered in init(); the redis driver registers
// itself when the redis_tracker package is imported.
//
// Factories receive the storage-driver options merged with the guardrails
// policy under well-known keys:
//
//	policy_max_attempts int
//	policy_window       time.Duration
//	policy_penalty      time.Duration
//
// The split keeps the registry's factory signature generic while still
// letting each backend see the policy it needs to enforce.
var TrackerRegistry = registry.NewRegistry[Tracker]()

func init() {
	TrackerRegistry.Register("memory", newMemoryTracker)
}

func newMemoryTracker(opts map[string]any) (Tracker, error) {
	return NewInjectionTracker(trackerPolicyFromOpts(opts)), nil
}

func trackerPolicyFromOpts(opts map[string]any) TrackerConfig {
	if cfg, ok := opts["_policy"].(TrackerConfig); ok {
		return cfg
	}
	def := DefaultTrackerConfig()
	return TrackerConfig{
		MaxAttempts: intOption(opts, "policy_max_attempts", def.MaxAttempts),
		Window:      durationOption(opts, "policy_window", def.Window),
		Penalty:     durationOption(opts, "policy_penalty", def.Penalty),
	}
}

func intOption(opts map[string]any, key string, def int) int {
	if opts == nil {
		return def
	}
	switch v := opts[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}

func stringOption(opts map[string]any, key, def string) string {
	if opts == nil {
		return def
	}
	if v, ok := opts[key].(string); ok && v != "" {
		return v
	}
	return def
}

func boolOption(opts map[string]any, key string, def bool) bool {
	if opts == nil {
		return def
	}
	if v, ok := opts[key].(bool); ok {
		return v
	}
	return def
}

func durationOption(opts map[string]any, key string, def time.Duration) time.Duration {
	if opts == nil {
		return def
	}
	switch v := opts[key].(type) {
	case time.Duration:
		return v
	case int64:
		return time.Duration(v)
	case float64:
		return time.Duration(v)
	}
	return def
}
