package usage

import "example.com/light-llm-gateway/internal/registry"

// Registry maps configured usage sink driver names to Sink factories.
//
// Third-party packages register additional drivers in their own init(); the
// resolved driver name comes from config (usage.driver) or from explicit
// Deps.UsageSink injection. The "audit" driver is intentionally NOT registered
// here because building it requires a *slog.Logger; server.go installs it as
// the default when no driver is configured and no Deps sink is provided.
var Registry = registry.NewRegistry[Sink]()

func init() {
	Registry.Register("noop", func(_ map[string]any) (Sink, error) { return NoopSink{}, nil })
}
