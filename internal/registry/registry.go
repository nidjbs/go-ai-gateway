// Package registry provides a small generic driver registry.
//
// A Registry[T] holds named factories that produce instances of T from a raw
// configuration map. It is the extension point third-party packages use to
// plug alternative storage backends (rate limiter, quota store, usage sink,
// ...) into the gateway without the core package taking a hard dependency on
// any specific implementation.
//
// Typical wiring:
//
//	import "example.com/light-llm-gateway/internal/registry"
//
//	type Factory[T any] func(rawConfig map[string]any) (T, error)
//
//	var LimiterRegistry = registry.NewRegistry[Limiter]()
//
//	func init() {
//	    LimiterRegistry.Register("memory", func(map[string]any) (Limiter, error) {
//	        return NewMemoryLimiter(), nil
//	    })
//	}
//
// Third-party packages can register their own driver in their own init() and
// the binary picks it up via the config driver name.
package registry

import (
	"fmt"
	"sort"
	"sync"
)

// Factory builds a driver instance from its raw configuration block.
// Each registered driver knows how to decode its own config keys.
type Factory[T any] func(rawConfig map[string]any) (T, error)

// Registry is a concurrent-safe map from driver name to Factory.
type Registry[T any] struct {
	mu      sync.RWMutex
	drivers map[string]Factory[T]
}

// NewRegistry returns a Registry with no drivers registered.
func NewRegistry[T any]() *Registry[T] {
	return &Registry[T]{drivers: make(map[string]Factory[T])}
}

// Register stores factory under name. If a driver with the same name already
// exists it is silently overwritten; this makes test-time replacement of
// production drivers straightforward and avoids forcing init() ordering.
func (r *Registry[T]) Register(name string, factory Factory[T]) {
	if name == "" || factory == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drivers[name] = factory
}

// Lookup returns the factory for name. The second return is false when the
// driver has not been registered.
func (r *Registry[T]) Lookup(name string) (Factory[T], bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.drivers[name]
	return f, ok
}

// Build is a convenience for Lookup + invoke. It returns an error when the
// driver is unknown; callers can decide whether to fall back or surface it.
func (r *Registry[T]) Build(name string, rawConfig map[string]any) (T, error) {
	factory, ok := r.Lookup(name)
	var zero T
	if !ok {
		return zero, fmt.Errorf("driver %q not registered; available: %v", name, r.Names())
	}
	return factory(rawConfig)
}

// Names returns the registered driver names in lexicographic order. The result
// is a fresh slice; callers may mutate it freely.
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.drivers))
	for name := range r.drivers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
