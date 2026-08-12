package usage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"
)

// AuditSink writes each Event as a single structured slog record. The default
// level is Info; failed events (Success=false) are upgraded to Warn so that
// log-based alerting can pick them up without scanning every record.
//
// This is the production default; binaries that want a different backend can
// either replace Deps.UsageSink or register an alternative driver under
// usage.Registry and select it from config.
type AuditSink struct {
	logger *slog.Logger
	level  slog.Level
	now    func() time.Time
}

// NewAuditSink returns a sink that writes to logger at the given level. Pass
// slog.LevelInfo (the typical choice) so every event lands in the audit trail.
func NewAuditSink(logger *slog.Logger) *AuditSink {
	return &AuditSink{logger: logger, level: slog.LevelInfo, now: time.Now}
}

// WithLevel returns a copy of the sink that emits at level instead of the
// default Info. Useful for tests that want to assert against a fixed level.
func (a *AuditSink) WithLevel(level slog.Level) *AuditSink {
	cp := *a
	cp.level = level
	return &cp
}

func (a *AuditSink) Record(_ context.Context, e Event) error {
	level := a.level
	if !e.Success && level < slog.LevelWarn {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("event_id", e.EventID),
		slog.String("request_id", e.RequestID),
		slog.String("endpoint", e.Endpoint),
		slog.String("alias", e.Alias),
		slog.String("requested_model", e.RequestedModel),
		slog.String("resolved_model", e.ResolvedModel),
		slog.String("provider", e.Provider),
		slog.String("upstream_model", e.UpstreamModel),
		slog.String("api_key_id", e.APIKeyID),
		slog.String("team_id", e.TeamID),
		slog.String("error_type", e.ErrorType),
		slog.String("stream_outcome", e.StreamOutcome),
		slog.Int("status", e.StatusCode),
		slog.Bool("success", e.Success),
		slog.Bool("streaming", e.Streaming),
		slog.Int("input_tokens", e.InputTokens),
		slog.Int("output_tokens", e.OutputTokens),
		slog.Int("total_tokens", e.TotalTokens),
		slog.Int64("duration_ms", e.DurationMS),
		slog.Int64("ttft_ms", e.TimeToFirstTokenMS),
		slog.Int("attempts", e.AttemptCount),
		slog.Int("retries", e.RetryCount),
		slog.Int("failovers", e.FailoverCount),
		slog.String("client_ip", e.ClientIP),
		slog.String("user_agent", e.UserAgent),
	}
	a.logger.LogAttrs(context.Background(), level, "audit", attrs...)
	return nil
}

// NewEventID returns a 32-character hex event identifier. It uses crypto/rand
// to avoid pulling in a uuid dependency for a single 16-byte identifier.
func NewEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
