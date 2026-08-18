// Package registry provides a small generic driver registry for plugging alternative storage backends into the gateway.
package registry

import (
	"fmt"
	"sort"
	"sync"
)

// Factory builds a driver instance from its raw configuration block.
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

// Register stores factory under name. Existing drivers with the same name are overwritten (helps test-time replacement).
func (r *Registry[T]) Register(name string, factory Factory[T]) {
	if name == "" || factory == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drivers[name] = factory
}

// Lookup returns the factory for name; the second return is false when the driver is not registered.
func (r *Registry[T]) Lookup(name string) (Factory[T], bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.drivers[name]
	return f, ok
}

// Build is Lookup + invoke; returns an error when the driver is unknown.
func (r *Registry[T]) Build(name string, rawConfig map[string]any) (T, error) {
	factory, ok := r.Lookup(name)
	var zero T
	if !ok {
		return zero, fmt.Errorf("driver %q not registered; available: %v", name, r.Names())
	}
	return factory(rawConfig)
}

// Names returns registered driver names in lexicographic order; the result is a fresh slice.
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
