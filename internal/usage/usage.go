package usage

import (
	"context"
	"time"
)

type Event struct {
	RequestID     string
	Endpoint      string
	Alias         string
	Provider      string
	UpstreamModel string
	StatusCode    int
	StartedAt     time.Time
	CompletedAt   time.Time
	InputTokens   int
	OutputTokens  int
	Streaming     bool
}

type Sink interface {
	Record(context.Context, Event) error
}

type NoopSink struct{}

func (NoopSink) Record(context.Context, Event) error { return nil }
