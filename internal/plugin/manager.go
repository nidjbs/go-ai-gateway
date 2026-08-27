package plugin

import (
	"errors"
	"log/slog"
)

// errorFailsOpen reports whether an ERROR from a plugin of the given type is
// swallowed so the request can continue.
//
// Only observability plugins qualify: logging and metrics watch the request,
// they do not gate it, so a dead sink must never take down the request path.
// Everything that participates in the decision fails closed — a guardrail that
// could not run has approved nothing. A deliberate rejection (RejectionError)
// is always honored whatever the plugin's type.
func errorFailsOpen(t PluginType) bool {
	switch t {
	case PluginTypeLogging, PluginTypeMetrics:
		return true
	default:
		return false
	}
}

// Manager owns the plugin chains for each stage and applies the failure policy.
// It is safe for concurrent use once configured.
type Manager struct {
	log    *slog.Logger
	before []Plugin
	after  []Plugin
	onErr  []Plugin
}

// NewManager returns an empty manager. A nil logger falls back to slog.Default.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log}
}

// Add registers a plugin on every stage interface it implements. A nil plugin
// is ignored.
func (m *Manager) Add(p Plugin) {
	if p == nil {
		return
	}
	if _, ok := p.(BeforeRequest); ok {
		m.before = append(m.before, p)
	}
	if _, ok := p.(AfterRequest); ok {
		m.after = append(m.after, p)
	}
	if _, ok := p.(OnError); ok {
		m.onErr = append(m.onErr, p)
	}
}

// Len returns the total number of plugin registrations across all stages.
func (m *Manager) Len() int {
	return len(m.before) + len(m.after) + len(m.onErr)
}

// RunBefore runs the before_request chain. It returns the first RejectionError
// (a denial) or FailureError (a broken fail-closed plugin); broken fail-open
// plugins are logged and skipped.
func (m *Manager) RunBefore(ctx *Context) error {
	return m.run(m.before, StageBeforeRequest, ctx)
}

// RunAfter runs the after_request chain.
func (m *Manager) RunAfter(ctx *Context) error {
	return m.run(m.after, StageAfterRequest, ctx)
}

// RunOnError runs the on_error chain after a failed request.
func (m *Manager) RunOnError(ctx *Context) error {
	return m.run(m.onErr, StageOnError, ctx)
}

func (m *Manager) run(stage []Plugin, stageName Stage, ctx *Context) error {
	for _, p := range stage {
		var err error
		switch stageName {
		case StageBeforeRequest:
			err = p.(BeforeRequest).BeforeRequest(ctx)
		case StageAfterRequest:
			err = p.(AfterRequest).AfterRequest(ctx)
		case StageOnError:
			err = p.(OnError).OnError(ctx)
		}
		if err == nil {
			continue
		}
		var re *RejectionError
		if errors.As(err, &re) {
			return re
		}
		if errorFailsOpen(p.Type()) {
			m.log.Warn("fail-open plugin error ignored", "plugin", p.Name(), "type", p.Type(), "stage", stageName, "error", err)
			continue
		}
		return &FailureError{Plugin: p.Name(), PluginType: p.Type(), Stage: stageName, Err: err}
	}
	return nil
}
