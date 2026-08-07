package usage

import (
	"context"
	"time"
)

type Event struct {
	EventID            string
	RequestID          string
	APIKeyID           string
	TeamID             string
	Endpoint           string
	Alias              string
	RequestedModel     string
	ResolvedModel      string
	Provider           string
	UpstreamModel      string
	ErrorType          string
	StreamOutcome      string
	StatusCode         int
	Success            bool
	Streaming          bool
	AttemptCount       int
	RetryCount         int
	FailoverCount      int
	InputTokens        int
	OutputTokens       int
	TotalTokens        int
	InputCostMicros    int64
	OutputCostMicros   int64
	TotalCostMicros    int64
	DurationMS         int64
	TimeToFirstTokenMS int64
	StartedAt          time.Time
	CompletedAt        time.Time
}

type Sink interface {
	Record(context.Context, Event) error
}

type NoopSink struct{}

func (NoopSink) Record(context.Context, Event) error { return nil }
