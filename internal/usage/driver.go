package usage

import "github.com/nidjbs/go-ai-gateway/internal/registry"

// Registry maps configured sink driver names to Sink factories.
// The "audit" driver is not registered here because it requires a *slog.Logger
// and is installed as the default by server.go.
var Registry = registry.NewRegistry[Sink]()

func init() {
	Registry.Register("noop", func(_ map[string]any) (Sink, error) { return NoopSink{}, nil })
}
