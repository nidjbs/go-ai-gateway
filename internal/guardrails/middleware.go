package guardrails

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/apierr"
	"github.com/nidjbs/go-ai-gateway/internal/auth"
)

// ctxKey is the context-key type for guardrails scan results.
type ctxKey string

const (
	ctxKeyScanResult ctxKey = "guardrails_scan_result"
	ctxKeyCanary     ctxKey = "guardrails_canary"
)

// Config is the guardrails middleware configuration.
type Config struct {
	Enabled          bool    // whether guardrails are active
	Mode             string  // "flag" | "block" | "off"
	Threshold        float64 // injection-score threshold (0.0–1.0)
	CanaryEnabled    bool    // whether outbound canary token detection is active
	CanaryBufferSize int     // outbound detection buffer size in bytes
	Tracker          TrackerConfig
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:          true,
		Mode:             "flag",
		Threshold:        0.75,
		CanaryEnabled:    true,
		CanaryBufferSize: 2048,
		Tracker:          DefaultTrackerConfig(),
	}
}

// Middleware is the guardrails HTTP middleware.
type Middleware struct {
	config  Config
	scanner *Scanner
	tracker Tracker
	logger  *slog.Logger
}

// NewMiddleware builds the middleware around the supplied tracker. Callers
// (typically the gateway server) resolve the tracker from TrackerRegistry
// so a distributed backend can be plugged in without touching this package.
func NewMiddleware(cfg Config, tracker Tracker, logger *slog.Logger) *Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	if tracker == nil {
		tracker = NewInjectionTracker(cfg.Tracker)
	}
	return &Middleware{
		config:  cfg,
		scanner: NewScanner(),
		tracker: tracker,
		logger:  logger,
	}
}

// Handle wraps the next handler with guardrails inspection.
func (m *Middleware) Handle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.config.Enabled || m.config.Mode == "off" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/embeddings" {
			next.ServeHTTP(w, r)
			return
		}

		body, err := readAndRestoreBody(r)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		messages, ok := MessagesFromChatRequest(body)
		if !ok || len(messages) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		result := m.scanner.ScanMessages(messages, m.config.Threshold)

		keyID := ""
		if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
			keyID = principal.APIKeyID
		}

		now := time.Now()
		if m.tracker.IsBlocked(keyID, now) {
			penalty := m.tracker.PenaltyRemaining(keyID, now)
			m.logger.Warn("guardrails: key blocked by injection tracker",
				"key_id", keyID,
				"penalty_remaining", penalty.Seconds(),
			)
			w.Header().Set("Retry-After", formatRetryAfter(penalty))
			apierr.Write(w, http.StatusTooManyRequests,
				"injection_tracker_blocked",
				"security_error",
				"API key temporarily blocked due to repeated security violations")
			return
		}

		if result.Action == "block" && m.config.Mode == "block" {
			blocked := m.tracker.Record(keyID, now)
			m.logger.Warn("guardrails: prompt injection blocked",
				"key_id", keyID,
				"score", result.Score,
				"matched_count", len(result.Matched),
				"tracker_blocked", blocked,
			)
			apierr.Write(w, http.StatusTooManyRequests,
				"prompt_injection_detected",
				"security_error",
				"Request blocked by security policy")
			return
		}

		if result.Action == "flag" || (result.Action == "block" && m.config.Mode == "flag") {
			m.logger.Info("guardrails: prompt injection flagged",
				"key_id", keyID,
				"score", result.Score,
				"matched_count", len(result.Matched),
			)
			ctx := context.WithValue(r.Context(), ctxKeyScanResult, result)
			r = r.WithContext(ctx)
		}

		next.ServeHTTP(w, r)
	})
}

// ScanResultFromContext retrieves the guardrails scan result from the context.
func ScanResultFromContext(ctx context.Context) (ScanResult, bool) {
	v := ctx.Value(ctxKeyScanResult)
	if v == nil {
		return ScanResult{}, false
	}
	result, ok := v.(ScanResult)
	return result, ok
}

// CanaryFromContext retrieves the canary token from the context.
func CanaryFromContext(ctx context.Context) (CanaryToken, bool) {
	v := ctx.Value(ctxKeyCanary)
	if v == nil {
		return CanaryToken{}, false
	}
	canary, ok := v.(CanaryToken)
	return canary, ok
}

// SetCanaryToContext stores the canary token in the context.
func SetCanaryToContext(ctx context.Context, canary CanaryToken) context.Context {
	return context.WithValue(ctx, ctxKeyCanary, canary)
}

func readAndRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

func formatRetryAfter(d time.Duration) string {
	sec := int(d.Seconds())
	if sec < 1 {
		sec = 1
	}
	return fmt.Sprintf("%d", sec)
}
