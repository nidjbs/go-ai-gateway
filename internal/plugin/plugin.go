// Package plugin defines a minimal middleware framework that runs around each
// routed request. A plugin attaches to one or more stages of the request
// lifecycle; the Manager executes the chain for a stage and applies the
// fail-open/fail-closed policy when a plugin breaks.
//
// A plugin either inspects/rewrites the request before it reaches a provider,
// screens/records the response after, or notes failures — enough to build
// guardrails, caching, rate limiting, and logging without touching the routing
// core.
package plugin

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/provider"
	"github.com/nidjbs/go-ai-gateway/internal/routing"
)

// PluginType classifies what a plugin does. It drives the failure policy: only
// observability types fail open, everything else fails closed.
type PluginType string

const (
	PluginTypeGuardrail PluginType = "guardrail"
	PluginTypeTransform PluginType = "transform"
	PluginTypeLogging   PluginType = "logging"
	PluginTypeMetrics   PluginType = "metrics"
	PluginTypeRatelimit PluginType = "ratelimit"
)

// Stage names a point in the request lifecycle a plugin can attach to.
type Stage string

const (
	StageBeforeRequest Stage = "before_request"
	StageAfterRequest  Stage = "after_request"
	StageOnError       Stage = "on_error"
)

// Plugin is a middleware unit. A plugin implements one or more of the optional
// stage interfaces (BeforeRequest, AfterRequest, OnError) and is registered on
// those stages when added to a Manager.
type Plugin interface {
	Name() string
	Type() PluginType
}

// BeforeRequest runs after the request is parsed but before a provider is
// called. It may reject the request by returning a *RejectionError, or rewrite
// ctx.Body.
type BeforeRequest interface {
	BeforeRequest(ctx *Context) error
}

// AfterRequest runs once a successful response is available. It may screen or
// record it; for non-streaming requests it may also rewrite ctx.Body.
type AfterRequest interface {
	AfterRequest(ctx *Context) error
}

// OnError runs when a request failed. It may record the failure from ctx.Err.
type OnError interface {
	OnError(ctx *Context) error
}

// Context carries per-request state a plugin stage may inspect or mutate.
type Context struct {
	// Endpoint is the gateway surface: "chat.completions", "responses", or
	// "embeddings".
	Endpoint string
	// Alias is the client-visible model name the request named.
	Alias string
	// APIKeyID is the authenticated tenant key id ("" when unauthenticated).
	APIKeyID string
	// Candidate is the provider candidate the request resolved to; empty
	// before routing has chosen one.
	Candidate routing.Candidate
	// Body is the request body for before_request and the response body for
	// after_request. Streaming responses cannot be rewritten as a whole, so
	// Body is left empty there.
	Body []byte
	// Status is the response status code seen by after_request/on_error.
	Status int
	// Usage carries upstream token accounting for after_request.
	Usage provider.Usage
	// Err carries the request failure for on_error.
	Err error

	values map[string]any
}

// Value returns a value a previous plugin stored, or nil.
func (c *Context) Value(key string) any {
	if c.values == nil {
		return nil
	}
	return c.values[key]
}

// SetValue stores a value for a later stage or plugin in the same request.
func (c *Context) SetValue(key string, v any) {
	if c.values == nil {
		c.values = make(map[string]any)
	}
	c.values[key] = v
}

// RejectionError is a plugin's deliberate denial — a guardrail tripping, a
// budget spent. It is a normal client response (4xx/429), not an error.
type RejectionError struct {
	Plugin string
	// StatusCode is the HTTP status to return; 0 means 400.
	StatusCode int
	// Code is the machine-readable error code in the response envelope.
	Code string
	// Type is the envelope error type; default "invalid_request_error".
	Type string
	// Reason is the human-readable message surfaced to the client.
	Reason string
	// RetryAfter, when positive, sets the Retry-After response header.
	RetryAfter time.Duration
}

func (e *RejectionError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("request rejected by plugin %s", e.Plugin)
	}
	return e.Reason
}

// Status returns the effective HTTP status for the rejection.
func (e *RejectionError) Status() int {
	if e.StatusCode != 0 {
		return e.StatusCode
	}
	return http.StatusBadRequest
}

// FailureError is a plugin that broke while running. Whether it is surfaced
// depends on the plugin type: observability plugins fail open (the request
// continues), everything else fails closed (the request is refused).
type FailureError struct {
	Plugin     string
	PluginType PluginType
	Stage      Stage
	Err        error
}

func (e *FailureError) Error() string {
	return fmt.Sprintf("plugin %s (%s) failed at %s: %v", e.Plugin, e.PluginType, e.Stage, e.Err)
}

func (e *FailureError) Unwrap() error { return e.Err }

// AsRejection reports whether err is a plugin rejection.
func AsRejection(err error) (*RejectionError, bool) {
	var re *RejectionError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}
