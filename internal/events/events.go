// Package events emits structured lifecycle events for the request pipeline and
// delivers them to in-process subscribers and configured webhooks. Emission is
// non-blocking: a slow or broken subscriber or webhook never affects the
// request path.
package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// EventType names a lifecycle milestone in the request pipeline.
type EventType string

const (
	TypeRequestStarted   EventType = "request.started"   // request parsed and about to route
	TypeRequestRejected  EventType = "request.rejected"  // denied: rate limit, quota, plugin, invalid, DLP
	TypeProviderAttempt  EventType = "provider.attempt"  // one upstream attempt (retry/failover visible)
	TypeRequestCompleted EventType = "request.completed" // success, incl. stream terminal
	TypeRequestFailed    EventType = "request.failed"    // error, incl. stream terminal
	TypeDLPHit           EventType = "dlp.hit"           // output DLP detection
)

// IsKnownType reports whether t is a valid event type.
func IsKnownType(t EventType) bool {
	switch t {
	case TypeRequestStarted, TypeRequestRejected, TypeProviderAttempt,
		TypeRequestCompleted, TypeRequestFailed, TypeDLPHit:
		return true
	}
	return false
}

// Event is one structured lifecycle event. RequestID correlates every event of
// a single request; fields are zeroed when not applicable to the type.
type Event struct {
	EventID             string         `json:"event_id"`
	RequestID           string         `json:"request_id,omitempty"`
	Type                EventType      `json:"type"`
	OccurredAt          time.Time      `json:"occurred_at"`
	Endpoint            string         `json:"endpoint,omitempty"`
	Alias               string         `json:"alias,omitempty"`
	APIKeyID            string         `json:"api_key_id,omitempty"`
	TeamID              string         `json:"team_id,omitempty"`
	Provider            string         `json:"provider,omitempty"`
	Model               string         `json:"model,omitempty"`
	UpstreamModel       string         `json:"upstream_model,omitempty"`
	StatusCode          int            `json:"status_code,omitempty"`
	ErrorType           string         `json:"error_type,omitempty"`
	StreamOutcome       string         `json:"stream_outcome,omitempty"`
	Attempt             int            `json:"attempt,omitempty"`
	Retry               bool           `json:"retry,omitempty"`
	Failover            bool           `json:"failover,omitempty"`
	InputTokens         int            `json:"input_tokens,omitempty"`
	OutputTokens        int            `json:"output_tokens,omitempty"`
	TotalTokens         int            `json:"total_tokens,omitempty"`
	CacheReadTokens     int            `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int            `json:"cache_creation_tokens,omitempty"`
	DurationMS          int64          `json:"duration_ms,omitempty"`
	Message             string         `json:"message,omitempty"`
	Payload             map[string]any `json:"payload,omitempty"`
}

// Subscriber is an in-process consumer of emitted events. Subscribers run
// asynchronously; a panic is recovered and never propagates to the caller.
type Subscriber interface {
	Name() string
	OnEvent(Event)
}

// Metrics lets the events package report its own health. Implemented by the
// gateway's metrics.Recorder.
type Metrics interface {
	EventEmitted(ctx context.Context, eventType string)
	EventWebhookDelivered(ctx context.Context, webhook string)
	EventWebhookFailed(ctx context.Context, webhook string)
	EventWebhookDropped(ctx context.Context, webhook string)
}

// NewEventID returns a 32-hex-char unique event id.
func NewEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
