package guardrails

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/plugin"
)

// Plugin adapts the prompt-injection scanner to the plugin framework's
// before_request stage, replacing the previous HTTP middleware as the gateway's
// execution path. The middleware form is kept for tests and embedding.
//
// It rejects (RejectionError) the request when the tracker has blocked the key
// or a scan in block mode exceeds the threshold; flag mode logs and stashes the
// scan result for later plugins.
type Plugin struct {
	config  Config
	scanner *Scanner
	tracker Tracker
	logger  *slog.Logger
}

// NewPlugin builds the guardrails before_request plugin around the supplied
// tracker. A nil logger falls back to slog.Default; a nil tracker builds the
// default in-process one.
func NewPlugin(cfg Config, tracker Tracker, logger *slog.Logger) *Plugin {
	if logger == nil {
		logger = slog.Default()
	}
	if tracker == nil {
		tracker = NewInjectionTracker(cfg.Tracker)
	}
	return &Plugin{config: cfg, scanner: NewScanner(), tracker: tracker, logger: logger}
}

func (p *Plugin) Name() string            { return "guardrails" }
func (p *Plugin) Type() plugin.PluginType { return plugin.PluginTypeGuardrail }

func (p *Plugin) BeforeRequest(ctx *plugin.Context) error {
	if !p.config.Enabled || p.config.Mode == "off" {
		return nil
	}
	var messages []Message
	var ok bool
	switch ctx.Endpoint {
	case "chat.completions":
		messages, ok = MessagesFromChatRequest(ctx.Body)
	case "responses":
		messages, ok = MessagesFromResponsesRequest(ctx.Body)
	default:
		// Embeddings and pass-through surfaces have no conversational text.
		return nil
	}
	if !ok || len(messages) == 0 {
		return nil
	}
	if allowlistedMessages(messages, p.config.Allowlist) {
		return nil
	}

	result := p.scanner.ScanMessages(messages, p.config.Threshold)

	now := time.Now()
	if p.tracker.IsBlocked(ctx.APIKeyID, now) {
		penalty := p.tracker.PenaltyRemaining(ctx.APIKeyID, now)
		p.logger.Warn("guardrails: key blocked by injection tracker",
			"key_id", ctx.APIKeyID,
			"penalty_remaining", penalty.Seconds(),
		)
		return &plugin.RejectionError{
			Plugin: "guardrails", StatusCode: http.StatusTooManyRequests,
			Code: "injection_tracker_blocked", Type: "security_error",
			Reason:     "API key temporarily blocked due to repeated security violations",
			RetryAfter: penalty,
		}
	}

	if result.Action == "block" && p.config.Mode == "block" {
		blocked := p.tracker.Record(ctx.APIKeyID, now)
		p.logger.Warn("guardrails: prompt injection blocked",
			"key_id", ctx.APIKeyID,
			"score", result.Score,
			"matched_count", len(result.Matched),
			"tracker_blocked", blocked,
		)
		return &plugin.RejectionError{
			Plugin: "guardrails", StatusCode: http.StatusTooManyRequests,
			Code: "prompt_injection_detected", Type: "security_error",
			Reason: "Request blocked by security policy",
		}
	}

	if result.Action == "flag" || (result.Action == "block" && p.config.Mode == "flag") {
		p.logger.Info("guardrails: prompt injection flagged",
			"key_id", ctx.APIKeyID,
			"score", result.Score,
			"matched_count", len(result.Matched),
		)
		ctx.SetValue("guardrails_scan_result", result)
	}
	return nil
}
