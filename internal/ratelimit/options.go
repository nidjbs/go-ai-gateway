package ratelimit

import "time"

// These helpers decode driver options with sensible defaults. Each
// implementation understands only the fields it advertises; the helpers
// just smooth out the YAML → Go type conversions.

func stringOption(opts map[string]any, key, def string) string {
	if opts == nil {
		return def
	}
	if v, ok := opts[key].(string); ok && v != "" {
		return v
	}
	return def
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
