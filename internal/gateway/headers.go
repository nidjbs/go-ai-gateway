package gateway

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/ratelimit"
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

func writeQuotaHeaders(w http.ResponseWriter, status ratelimit.QuotaStatus) {
	if status.Limit <= 0 {
		return
	}
	w.Header().Set("X-Quota-Limit-Tokens", strconv.FormatInt(status.Limit, 10))
	w.Header().Set("X-Quota-Used-Tokens", strconv.FormatInt(status.Used, 10))
	w.Header().Set("X-Quota-Remaining-Tokens", strconv.FormatInt(status.Remaining, 10))
	if !status.ResetAt.IsZero() {
		w.Header().Set("X-Quota-Reset-At", status.ResetAt.UTC().Format(time.RFC3339))
	}
}
