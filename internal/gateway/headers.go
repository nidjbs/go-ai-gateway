package gateway

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/config"
	"github.com/nidjbs/go-ai-gateway/internal/ratelimit"
)

func writeRateLimitHeaders(w http.ResponseWriter, limits config.KeyLimits, decision ratelimit.Decision) {
	if limits.RPS <= 0 {
		return
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limits.Burst))
	if decision.Remaining >= 0 {
		w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(decision.Remaining, 10))
	}
	if !decision.Allowed && decision.RetryAfter > 0 {
		seconds := int64(math.Ceil(decision.RetryAfter.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
}

// writeQuotaHeaders emits X-Quota-* response headers for the supplied status.
func writeQuotaHeaders(w http.ResponseWriter, status ratelimit.QuotaStatus) {
	if status.Limit <= 0 {
		return
	}
	switch status.Window {
	case ratelimit.WindowMonthly:
		w.Header().Set("X-Quota-Monthly-Limit-Tokens", strconv.FormatInt(status.Limit, 10))
		w.Header().Set("X-Quota-Monthly-Used-Tokens", strconv.FormatInt(status.Used, 10))
		w.Header().Set("X-Quota-Monthly-Remaining-Tokens", strconv.FormatInt(status.Remaining, 10))
		if !status.ResetAt.IsZero() {
			w.Header().Set("X-Quota-Monthly-Reset-At", status.ResetAt.UTC().Format(time.RFC3339))
		}
	default:
		// Daily quota; X-Quota-Alias header disambiguates per-alias from key-level.
		w.Header().Set("X-Quota-Limit-Tokens", strconv.FormatInt(status.Limit, 10))
		w.Header().Set("X-Quota-Used-Tokens", strconv.FormatInt(status.Used, 10))
		w.Header().Set("X-Quota-Remaining-Tokens", strconv.FormatInt(status.Remaining, 10))
		if !status.ResetAt.IsZero() {
			w.Header().Set("X-Quota-Reset-At", status.ResetAt.UTC().Format(time.RFC3339))
		}
	}
}

// writeAliasQuotaTag records which alias the daily quota headers refer to.
// Only emitted when the handler is enforcing a per-alias quota.
func writeAliasQuotaTag(w http.ResponseWriter, alias string) {
	if alias == "" {
		return
	}
	w.Header().Set("X-Quota-Alias", alias)
}
