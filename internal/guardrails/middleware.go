package guardrails

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
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
	// Allowlist holds substrings that bypass scanning when present in the
	// message content — an escape hatch for false-positive-prone payloads.
	Allowlist []string
	// MaxBodyBytes is the request-body size up to which scanning reads and
	// restores the body. Larger bodies are forwarded untouched (scan skipped)
	// so the middleware never silently truncates a payload; the gateway's
	// own body limit rejects genuine over-limit requests downstream.
	MaxBodyBytes int64
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
		if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/responses" && r.URL.Path != "/v1/embeddings" {
			next.ServeHTTP(w, r)
			return
		}

		body, oversized, err := readAndRestoreBody(r, m.config.MaxBodyBytes)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		if oversized {
			// Never silently truncate a payload: forward the body untouched
			// (the gateway's body limit rejects over-limit requests) and skip
			// the scan rather than scanning a corrupt prefix.
			m.logger.Warn("guardrails: request body exceeds scan limit; skipping scan",
				"path", r.URL.Path,
				"limit_bytes", m.config.MaxBodyBytes,
			)
			next.ServeHTTP(w, r)
			return
		}

		var messages []Message
		var ok bool
		switch r.URL.Path {
		case "/v1/chat/completions":
			messages, ok = MessagesFromChatRequest(body)
		case "/v1/responses":
			messages, ok = MessagesFromResponsesRequest(body)
		default:
			// /v1/embeddings has no conversational messages to scan.
			next.ServeHTTP(w, r)
			return
		}
		if !ok || len(messages) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if m.allowlisted(messages) {
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

// allowlisted reports whether any message content contains a configured
// allowlist substring. It gives operators an escape hatch for payloads that
// reliably false-positive (benchmark suites, red-team exercises, structured
// tool data) without disabling the whole layer.
func (m *Middleware) allowlisted(messages []Message) bool {
	return allowlistedMessages(messages, m.config.Allowlist)
}

// allowlistedMessages is the shared allowlist check used by the middleware and
// the plugin form.
func allowlistedMessages(messages []Message, allowlist []string) bool {
	if len(allowlist) == 0 {
		return false
	}
	var buf strings.Builder
	for _, msg := range messages {
		buf.WriteString(msg.Content)
		buf.WriteByte('\n')
	}
	content := buf.String()
	for _, entry := range allowlist {
		if entry != "" && strings.Contains(content, entry) {
			return true
		}
	}
	return false
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

// defaultScanBodyBytes is the scan/restore limit when Config.MaxBodyBytes is
// unset (e.g. middleware constructed directly in tests).
const defaultScanBodyBytes = 1 << 20

// readAndRestoreBody reads up to limit+1 bytes so callers can distinguish a
// body that fits (len(data) <= limit) from one that exceeds the limit
// (oversized). The body stream is always restored — oversized or not — so the
// downstream handler still enforces its own limit instead of receiving a
// silently truncated payload.
func readAndRestoreBody(r *http.Request, limit int64) (data []byte, oversized bool, err error) {
	if r.Body == nil {
		return nil, false, nil
	}
	if limit <= 0 {
		limit = defaultScanBodyBytes
	}
	data, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
	r.Body.Close()
	if err != nil {
		return nil, false, err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, int64(len(data)) > limit, nil
}

func formatRetryAfter(d time.Duration) string {
	sec := int(d.Seconds())
	if sec < 1 {
		sec = 1
	}
	return fmt.Sprintf("%d", sec)
}
