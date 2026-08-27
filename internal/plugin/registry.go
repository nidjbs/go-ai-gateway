package plugin

import "fmt"

// Constructor builds a plugin from its configuration. Config values are
// decoded from the plugin's config block; a built-in plugin may instead carry
// its dependencies explicitly and register a zero-arg factory here.
type Constructor func(opts map[string]any) (Plugin, error)

var factories = map[string]Constructor{}

// Register names a plugin constructor so it can be built from configuration.
// Registering the same name twice is a programming error and panics at
// startup, before any request traffic.
func Register(name string, ctor Constructor) {
	if name == "" {
		panic("plugin: Register called with an empty name")
	}
	if ctor == nil {
		panic("plugin: Register called with a nil constructor for " + name)
	}
	if _, exists := factories[name]; exists {
		panic("plugin: Register called twice for " + name)
	}
	factories[name] = ctor
}

// Build constructs a named plugin from its options. The bool reports whether
// the name is registered; a nil error with ok=false means "no such plugin".
func Build(name string, opts map[string]any) (Plugin, error) {
	ctor, ok := factories[name]
	if !ok {
		return nil, fmt.Errorf("plugin %q is not registered", name)
	}
	return ctor(opts)
}
