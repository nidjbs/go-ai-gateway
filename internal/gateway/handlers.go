package gateway

import (
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/tidwall/sjson"

	"github.com/nidjbs/go-ai-gateway/internal/apierr"
	"github.com/nidjbs/go-ai-gateway/internal/auth"
	"github.com/nidjbs/go-ai-gateway/internal/circuitbreaker"
	"github.com/nidjbs/go-ai-gateway/internal/concurrency"
	"github.com/nidjbs/go-ai-gateway/internal/config"
	"github.com/nidjbs/go-ai-gateway/internal/dlp"
	"github.com/nidjbs/go-ai-gateway/internal/metrics"
	"github.com/nidjbs/go-ai-gateway/internal/provider"
	"github.com/nidjbs/go-ai-gateway/internal/ratelimit"
	"github.com/nidjbs/go-ai-gateway/internal/retry"
	"github.com/nidjbs/go-ai-gateway/internal/revocation"
	"github.com/nidjbs/go-ai-gateway/internal/routing"
	"github.com/nidjbs/go-ai-gateway/internal/usage"
)

const (
	maxRequestBodyBytes  = 1 << 20
	upstreamErrorMessage = "upstream request failed"
)

type handler struct {
	config         *config.Config
	logger         *slog.Logger
	authenticator  auth.Authenticator
	usageSink      usage.Sink
	provider       *provider.Client
	metrics        *metrics.Recorder
	limiter        ratelimit.Limiter
	quotaStore     ratelimit.QuotaStore
	breaker        circuitbreaker.Breaker
	keyConcurrency func(keyID string) *concurrency.Limiter
	now            func() time.Time
	apiKeyLimits   map[string]config.KeyLimits
	idemMu         *sync.Mutex
	idem           map[string]idemEntry
	revoker        revocation.Store
	dlpDetector    *dlp.Detector
}

// idemEntry is one cached successful response keyed by (api key, Idempotency-Key).
type idemEntry struct {
	status   int
	body     []byte
	storedAt time.Time
}

// usageRecord is the structured payload every handler passes to recordUsage.
type usageRecord struct {
	started       time.Time
	endpoint      string
	alias         string
	candidate     routing.Candidate
	usage         provider.Usage
	ttft          time.Time
	streaming     bool
	status        int
	errorType     string
	streamOutcome string
	attempts      retry.Attempts
}

// metricsRecord mirrors usageRecord for the metrics recorder.
type metricsRecord struct {
	started    time.Time
	endpoint   string
	alias      string
	candidate  routing.Candidate
	usage      provider.Usage
	firstToken time.Time
	status     int
	errorType  string
}

func (h handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := h.now()
		principal, err := h.authenticator.Authenticate(r.Context(), r)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthorized) {
				h.recordUsage(r.Context(), usageRecord{
					started: started, endpoint: requestEndpoint(r),
					status: http.StatusUnauthorized, errorType: "unauthorized",
				})
				apierr.Write(w, http.StatusUnauthorized, "invalid_api_key", "invalid_request_error", "Invalid API key")
				return
			}
			h.recordUsage(r.Context(), usageRecord{
				started: started, endpoint: requestEndpoint(r),
				status: http.StatusInternalServerError, errorType: "auth_internal",
			})
			apierr.Write(w, http.StatusInternalServerError, "authentication_error", "server_error", "Authentication failed")
			return
		}
		ctx := auth.ContextWithPrincipal(r.Context(), principal)
		recordAccessPrincipal(r.Context(), principal)
		if principal.APIKeyID != "" {
			if h.revoker != nil && h.revoker.IsRevoked(principal.APIKeyID) {
				h.recordUsage(r.Context(), usageRecord{
					started: started, endpoint: requestEndpoint(r),
					candidate: routing.Candidate{Name: principal.APIKeyID, Model: principal.APIKeyID},
					status:    http.StatusUnauthorized, errorType: "revoked_api_key",
				})
				apierr.Write(w, http.StatusUnauthorized, "revoked_api_key", "invalid_request_error", "API key has been revoked")
				return
			}
			if h.keyConcurrency != nil {
				kl := h.keyConcurrency(principal.APIKeyID)
				if kl != nil && !kl.TryAcquire() {
					h.recordUsage(r.Context(), usageRecord{
						started: started, endpoint: requestEndpoint(r),
						candidate: routing.Candidate{Name: principal.APIKeyID, Model: principal.APIKeyID},
						status:    http.StatusServiceUnavailable, errorType: "key_overloaded",
					})
					w.Header().Set("Retry-After", "1")
					apierr.Write(w, http.StatusServiceUnavailable, "key_overloaded", "server_error", "API key concurrency limit exceeded")
					return
				}
				if kl != nil {
					defer kl.Release()
				}
			}
			limits := h.apiKeyLimits[principal.APIKeyID]
			if h.limiter != nil {
				decision := h.limiter.Allow(principal.APIKeyID, limits, h.now())
				writeRateLimitHeaders(w, limits, decision)
				if !decision.Allowed {
					h.recordUsage(ctx, usageRecord{
						started: started, endpoint: requestEndpoint(r),
						candidate: routing.Candidate{Name: principal.APIKeyID, Model: principal.APIKeyID},
						status:    http.StatusTooManyRequests, errorType: "rate_limit_exceeded",
					})
					apierr.Write(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_error", "API key rate limit exceeded")
					return
				}
			}
			if h.quotaStore != nil && limits.MaxRequestsPerDay > 0 {
				status := h.quotaStore.Charge(ratelimit.QuotaScope{KeyID: principal.APIKeyID, Window: ratelimit.WindowDaily, Metric: ratelimit.RequestsMetric}, limits.MaxRequestsPerDay, 1, h.now())
				if status.Used > limits.MaxRequestsPerDay {
					h.recordUsage(ctx, usageRecord{
						started: started, endpoint: requestEndpoint(r),
						candidate: routing.Candidate{Name: principal.APIKeyID, Model: principal.APIKeyID},
						status:    http.StatusTooManyRequests, errorType: "quota_exceeded_requests",
					})
					apierr.Write(w, http.StatusTooManyRequests, "quota_exceeded_requests", "rate_limit_error", "API key daily request quota exceeded")
					return
				}
			}
			if h.quotaStore != nil && limits.PredayTokens > 0 {
				status := h.quotaStore.Peek(ratelimit.QuotaScope{KeyID: principal.APIKeyID, Window: ratelimit.WindowDaily}, limits.PredayTokens, h.now())
				writeQuotaHeaders(w, status)
				if status.Remaining <= 0 {
					h.recordUsage(ctx, usageRecord{
						started: started, endpoint: requestEndpoint(r),
						candidate: routing.Candidate{Name: principal.APIKeyID, Model: principal.APIKeyID},
						status:    http.StatusTooManyRequests, errorType: "quota_exceeded",
					})
					apierr.Write(w, http.StatusTooManyRequests, "quota_exceeded", "rate_limit_error", "API key daily quota exceeded")
					return
				}
			}
			if h.quotaStore != nil && limits.MonthlyTokens > 0 {
				status := h.quotaStore.Peek(ratelimit.QuotaScope{KeyID: principal.APIKeyID, Window: ratelimit.WindowMonthly}, limits.MonthlyTokens, h.now())
				writeQuotaHeaders(w, status)
				if status.Remaining <= 0 {
					h.recordUsage(ctx, usageRecord{
						started: started, endpoint: requestEndpoint(r),
						candidate: routing.Candidate{Name: principal.APIKeyID, Model: principal.APIKeyID},
						status:    http.StatusTooManyRequests, errorType: "quota_exceeded_monthly",
					})
					apierr.Write(w, http.StatusTooManyRequests, "quota_exceeded_monthly", "rate_limit_error", "API key monthly quota exceeded")
					return
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h handler) models(w http.ResponseWriter, _ *http.Request) {
	names := h.config.AliasNames()
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: make([]model, 0, len(names))}
	for _, name := range names {
		out.Data = append(out.Data, model{ID: name, Object: "model", OwnedBy: "go-ai-gateway"})
	}
	writeJSON(w, http.StatusOK, out)
}

type chatRequest struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

// recordBadRequest writes a 400 and a validation_error audit event.
func (h handler) recordBadRequest(ctx context.Context, started time.Time, endpoint, alias string, candidate routing.Candidate) {
	h.recordUsage(ctx, usageRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		status: http.StatusBadRequest, errorType: "validation_error",
	})
}

func (h handler) writeInvalidRequest(w http.ResponseWriter, r *http.Request, started time.Time, alias string, candidate routing.Candidate, message string) {
	h.recordBadRequest(r.Context(), started, requestEndpoint(r), alias, candidate)
	apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", message)
}

func (h handler) chat(w http.ResponseWriter, r *http.Request) {
	body, request, candidates, ok := h.parseChat(w, r)
	if !ok {
		return
	}
	recordAccessAlias(r.Context(), request.Model)
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
		if !h.checkAliasQuota(w, r, h.now(), principal, request.Model) {
			return
		}
		body = h.capRequestTokens(principal, body)
	}
	if request.Stream {
		h.chatStream(w, r, body, request.Model, candidates)
		return
	}
	if h.replayIdempotent(w, r, "chat.completions", request.Model, h.now()) {
		return
	}
	started := h.now()
	adapterRequest := provider.Request{Operation: provider.ChatCompletions, Body: body}
	result, candidate, attempts, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, h.breaker, candidates, func(ctx context.Context, c routing.Candidate) (provider.Result, error) {
		return h.provider.Do(ctx, adapterRequest, c)
	})
	if err != nil {
		h.writeProviderError(w, r, started, "chat.completions", request.Model, candidate, false, err)
		return
	}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal.APIKeyID != "" {
		h.chargeQuota(w, principal.APIKeyID, request.Model, h.chargeableTokens(int64(result.Usage.Total()), body))
	}
	respBody := replaceModel(result.Body, request.Model)
	var proceed bool
	if respBody, proceed = h.dlpResponseBody(w, r, started, "chat.completions", request.Model, candidate, respBody); !proceed {
		return
	}
	writeRawJSON(w, http.StatusOK, respBody)
	h.recordUsage(r.Context(), usageRecord{
		started: started, endpoint: "chat.completions", alias: request.Model, candidate: candidate,
		usage:  result.Usage,
		status: http.StatusOK, attempts: attempts,
	})
	h.recordMetrics(r.Context(), metricsRecord{
		started: started, endpoint: "chat.completions", alias: request.Model, candidate: candidate,
		usage: result.Usage, status: http.StatusOK,
	})
	h.storeIdempotent(r, http.StatusOK, respBody)
	h.logger.Info("request complete", "request_id", requestID(r), "endpoint", "chat.completions", "provider", candidate.Name, "attempts", attempts.Total, "status", http.StatusOK, "upstream_duration_ms", time.Since(started).Milliseconds())
}

type responsesRequest struct {
	Model  string          `json:"model"`
	Input  json.RawMessage `json:"input"`
	Stream bool            `json:"stream"`
}

func (h handler) responses(w http.ResponseWriter, r *http.Request) {
	body, request, candidates, ok := h.parseResponses(w, r)
	if !ok {
		return
	}
	recordAccessAlias(r.Context(), request.Model)
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
		if !h.checkAliasQuota(w, r, h.now(), principal, request.Model) {
			return
		}
		body = h.capRequestTokens(principal, body)
	}
	if request.Stream {
		h.responsesStream(w, r, body, request.Model, candidates)
		return
	}
	if h.replayIdempotent(w, r, "responses", request.Model, h.now()) {
		return
	}
	started := h.now()
	adapterRequest := provider.Request{Operation: provider.Responses, Body: body}
	result, candidate, attempts, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, h.breaker, candidates, func(ctx context.Context, c routing.Candidate) (provider.Result, error) {
		return h.provider.Do(ctx, adapterRequest, c)
	})
	if err != nil {
		h.writeProviderError(w, r, started, "responses", request.Model, candidate, false, err)
		return
	}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal.APIKeyID != "" {
		h.chargeQuota(w, principal.APIKeyID, request.Model, h.chargeableTokens(int64(result.Usage.Total()), body))
	}
	respBody := replaceModel(result.Body, request.Model)
	var proceed bool
	if respBody, proceed = h.dlpResponseBody(w, r, started, "responses", request.Model, candidate, respBody); !proceed {
		return
	}
	writeRawJSON(w, http.StatusOK, respBody)
	h.recordUsage(r.Context(), usageRecord{started: started, endpoint: "responses", alias: request.Model, candidate: candidate, usage: result.Usage, status: http.StatusOK, attempts: attempts})
	h.recordMetrics(r.Context(), metricsRecord{started: started, endpoint: "responses", alias: request.Model, candidate: candidate, usage: result.Usage, status: http.StatusOK})
	h.storeIdempotent(r, http.StatusOK, respBody)
}

func (h handler) parseResponses(w http.ResponseWriter, r *http.Request) (json.RawMessage, responsesRequest, []routing.Candidate, bool) {
	started := h.now()
	var request responsesRequest
	body, err := readBody(w, r)
	if err != nil {
		h.writeInvalidRequest(w, r, started, "", routing.Candidate{}, err.Error())
		return nil, request, nil, false
	}
	if err := json.Unmarshal(body, &request); err != nil {
		h.writeInvalidRequest(w, r, started, "", routing.Candidate{}, err.Error())
		return nil, request, nil, false
	}
	if strings.TrimSpace(request.Model) == "" || len(request.Input) == 0 || string(request.Input) == "null" {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, "model and input are required")
		return nil, request, nil, false
	}
	candidates, err := routing.Resolve(h.config, request.Model)
	if err != nil {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, err.Error())
		return nil, request, nil, false
	}
	adapterRequest := provider.Request{Operation: provider.Responses, Body: body}
	candidates, err = h.provider.Filter(candidates, adapterRequest)
	if err != nil {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, err.Error())
		return nil, request, nil, false
	}
	if len(candidates) == 0 {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, "no configured provider supports responses")
		return nil, request, nil, false
	}
	return body, request, candidates, true
}

func (h handler) responsesStream(w http.ResponseWriter, r *http.Request, body json.RawMessage, alias string, candidates []routing.Candidate) {
	started := h.now()
	adapterRequest := provider.Request{Operation: provider.Responses, Body: body}
	stream, candidate, attempts, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, h.breaker, candidates, func(ctx context.Context, c routing.Candidate) (provider.Stream, error) {
		return h.provider.OpenStream(ctx, adapterRequest, c)
	})
	if err != nil {
		h.writeProviderError(w, r, started, "responses", alias, candidate, true, err)
		return
	}
	defer stream.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		apierr.Write(w, http.StatusInternalServerError, "streaming_unsupported", "server_error", "response streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	streamCtx, cancelStream := context.WithCancel(r.Context())
	defer cancelStream()
	idleFired, maxFired, resetIdle, stopWatchdogs := startStreamWatchdogs(h.config.Server.StreamIdleTimeout, h.config.Server.StreamMaxDuration, cancelStream)
	defer stopWatchdogs()

	var streamMasker *dlp.StreamMasker
	if h.dlpDetector != nil && h.dlpDetector.Enabled() {
		streamMasker = h.dlpDetector.NewStreamMasker()
	}
	seen := false
	var firstToken time.Time
	var emittedRunes int64
	for result := range pumpStream(streamCtx, stream) {
		resetIdle()
		if idleFired() || maxFired() {
			h.writeStreamTimeout(w, r, started, "responses", alias, candidate, firstToken, attempts, body, emittedRunes, idleFired, maxFired)
			return
		}
		if result.err != nil {
			err := result.err
			if err != io.EOF && !errors.Is(err, context.Canceled) {
				_, _ = fmt.Fprintf(w, "event: error\ndata: {\"error\":{\"message\":%q,\"type\":\"upstream_error\",\"code\":\"upstream_error\"}}\n\n", upstreamErrorMessage)
				flusher.Flush()
			}
			h.chargeStreamEstimate(w, r, alias, body, emittedRunes)
			errorType, streamOutcome, status := streamOutcomeFor(result.err, r.Context())
			h.recordUsage(r.Context(), usageRecord{started: started, endpoint: "responses", alias: alias, candidate: candidate, ttft: firstToken, streaming: true, status: status, errorType: errorType, streamOutcome: streamOutcome, attempts: attempts})
			h.recordMetrics(r.Context(), metricsRecord{started: started, endpoint: "responses", alias: alias, candidate: candidate, firstToken: firstToken, status: status, errorType: errorType})
			return
		}
		event := result.event
		if len(event.Data) > 0 {
			data := event.Data
			if streamMasker != nil {
				if h.dlpDetector.RejectMode() {
					if hits := streamMasker.Reject(data); len(hits) > 0 {
						h.writeDLPRejectStream(w, r, started, "responses", alias, candidate, firstToken, attempts, body, emittedRunes, hits)
						return
					}
				} else {
					data = streamMasker.Process(data)
				}
			}
			emittedRunes += int64(utf8.RuneCount(data))
			if event.Event != "" {
				_, _ = fmt.Fprintf(w, "event: %s\n", event.Event)
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", replaceModel(data, alias))
			flusher.Flush()
			if !seen {
				seen = true
				firstToken = h.now()
				h.recordUsage(r.Context(), usageRecord{started: started, endpoint: "responses", alias: alias, candidate: candidate, ttft: firstToken, streaming: true, status: http.StatusOK, streamOutcome: "first_token", attempts: attempts})
			}
		}
		if event.Done {
			if streamMasker != nil {
				if tail := streamMasker.Flush(); len(tail) > 0 {
					if h.dlpDetector.MaskMode() {
						_, _ = fmt.Fprintf(w, "data: %s\n\n", replaceModel(tail, alias))
						flusher.Flush()
					}
				}
			}
			if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
				h.chargeQuota(w, principal.APIKeyID, alias, int64(event.Usage.Total()))
			}
			h.recordUsage(r.Context(), usageRecord{started: started, endpoint: "responses", alias: alias, candidate: candidate, usage: event.Usage, ttft: firstToken, streaming: true, status: http.StatusOK, streamOutcome: "completed", attempts: attempts})
			h.recordMetrics(r.Context(), metricsRecord{started: started, endpoint: "responses", alias: alias, candidate: candidate, usage: event.Usage, firstToken: firstToken, status: http.StatusOK})
			return
		}
	}
	if streamMasker != nil {
		if tail := streamMasker.Flush(); len(tail) > 0 && h.dlpDetector.MaskMode() {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", replaceModel(tail, alias))
			flusher.Flush()
		}
	}
	if idleFired() || maxFired() {
		h.writeStreamTimeout(w, r, started, "responses", alias, candidate, firstToken, attempts, body, emittedRunes, idleFired, maxFired)
	}
}

func (h handler) chargeQuota(w http.ResponseWriter, keyID, alias string, delta int64) {
	if delta <= 0 {
		return
	}
	limits, ok := h.apiKeyLimits[keyID]
	if !ok {
		return
	}
	if h.quotaStore == nil {
		return
	}
	if limits.PredayTokens > 0 {
		status := h.quotaStore.Charge(ratelimit.QuotaScope{KeyID: keyID, Window: ratelimit.WindowDaily}, limits.PredayTokens, delta, h.now())
		writeQuotaHeaders(w, status)
	}
	if limits.MonthlyTokens > 0 {
		status := h.quotaStore.Charge(ratelimit.QuotaScope{KeyID: keyID, Window: ratelimit.WindowMonthly}, limits.MonthlyTokens, delta, h.now())
		writeQuotaHeaders(w, status)
	}
	if aliasLimit, ok := limits.AliasPredayTokens[alias]; ok && aliasLimit > 0 {
		status := h.quotaStore.Charge(ratelimit.QuotaScope{KeyID: keyID, Alias: alias, Window: ratelimit.WindowDaily}, aliasLimit, delta, h.now())
		writeAliasQuotaTag(w, alias)
		writeQuotaHeaders(w, status)
	}
}

// checkAliasQuota enforces the per-alias daily quota. Returns true if the
// request may proceed; false after writing a 429.
func (h handler) checkAliasQuota(w http.ResponseWriter, r *http.Request, started time.Time, principal auth.Principal, alias string) bool {
	if principal.APIKeyID == "" || h.quotaStore == nil {
		return true
	}
	limits, ok := h.apiKeyLimits[principal.APIKeyID]
	if !ok {
		return true
	}
	aliasLimit, ok := limits.AliasPredayTokens[alias]
	if !ok || aliasLimit <= 0 {
		return true
	}
	status := h.quotaStore.Peek(ratelimit.QuotaScope{KeyID: principal.APIKeyID, Alias: alias, Window: ratelimit.WindowDaily}, aliasLimit, h.now())
	writeAliasQuotaTag(w, alias)
	writeQuotaHeaders(w, status)
	if status.Remaining <= 0 {
		h.recordUsage(r.Context(), usageRecord{
			started: started, endpoint: requestEndpoint(r), alias: alias,
			candidate: routing.Candidate{Name: alias, Model: alias},
			status:    http.StatusTooManyRequests, errorType: "quota_exceeded_alias",
		})
		apierr.Write(w, http.StatusTooManyRequests, "quota_exceeded_alias", "rate_limit_error", "Alias daily quota exceeded")
		return false
	}
	return true
}

func (h handler) parseChat(w http.ResponseWriter, r *http.Request) (json.RawMessage, chatRequest, []routing.Candidate, bool) {
	started := h.now()
	var request chatRequest
	body, err := readBody(w, r)
	if err != nil {
		h.writeInvalidRequest(w, r, started, "", routing.Candidate{}, err.Error())
		return nil, request, nil, false
	}
	if err := json.Unmarshal(body, &request); err != nil {
		h.writeInvalidRequest(w, r, started, "", routing.Candidate{}, err.Error())
		return nil, request, nil, false
	}
	if strings.TrimSpace(request.Model) == "" || len(request.Messages) == 0 || string(request.Messages) == "null" {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, "model and messages are required")
		return nil, request, nil, false
	}
	candidates, err := routing.Resolve(h.config, request.Model)
	if err != nil {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, err.Error())
		return nil, request, nil, false
	}
	adapterRequest := provider.Request{Operation: provider.ChatCompletions, Body: body}
	candidates, err = h.provider.Filter(candidates, adapterRequest)
	if err != nil {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, err.Error())
		return nil, request, nil, false
	}
	if len(candidates) == 0 {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, "no configured provider supports chat.completions")
		return nil, request, nil, false
	}
	return body, request, candidates, true
}

func (h handler) chatStream(w http.ResponseWriter, r *http.Request, body json.RawMessage, alias string, candidates []routing.Candidate) {
	started := h.now()
	adapterRequest := provider.Request{Operation: provider.ChatCompletions, Body: body}
	stream, candidate, attempts, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, h.breaker, candidates, func(ctx context.Context, c routing.Candidate) (provider.Stream, error) {
		return h.provider.OpenStream(ctx, adapterRequest, c)
	})
	if err != nil {
		h.writeProviderError(w, r, started, "chat.completions", alias, candidate, true, err)
		return
	}
	defer stream.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		apierr.Write(w, http.StatusInternalServerError, "streaming_unsupported", "server_error", "response streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	streamCtx, cancelStream := context.WithCancel(r.Context())
	defer cancelStream()
	idleFired, maxFired, resetIdle, stopWatchdogs := startStreamWatchdogs(h.config.Server.StreamIdleTimeout, h.config.Server.StreamMaxDuration, cancelStream)
	defer stopWatchdogs()

	var firstToken time.Time
	var emittedRunes int64
	var streamMasker *dlp.StreamMasker
	if h.dlpDetector != nil && h.dlpDetector.Enabled() {
		streamMasker = h.dlpDetector.NewStreamMasker()
	}
	// pumpStream pushes events over a bounded channel, applying backpressure on slow clients.
	for result := range pumpStream(streamCtx, stream) {
		resetIdle()
		if idleFired() || maxFired() {
			h.writeStreamTimeout(w, r, started, "chat.completions", alias, candidate, firstToken, attempts, body, emittedRunes, idleFired, maxFired)
			return
		}
		if result.err != nil {
			err := result.err
			if err != io.EOF && !errors.Is(err, context.Canceled) {
				_, _ = fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":\"upstream_error\",\"code\":\"upstream_error\"}}\n\n", upstreamErrorMessage)
				flusher.Flush()
			}
			h.chargeStreamEstimate(w, r, alias, body, emittedRunes)
			errorType, streamOutcome, status := streamOutcomeFor(result.err, r.Context())
			h.recordUsage(r.Context(), usageRecord{
				started: started, endpoint: "chat.completions", alias: alias, candidate: candidate,
				ttft: firstToken, streaming: true,
				status: status, errorType: errorType, streamOutcome: streamOutcome, attempts: attempts,
			})
			h.recordMetrics(r.Context(), metricsRecord{
				started: started, endpoint: "chat.completions", alias: alias, candidate: candidate,
				firstToken: firstToken, status: status, errorType: errorType,
			})
			return
		}
		event := result.event
		if event.Done {
			if streamMasker != nil && h.dlpDetector.MaskMode() {
				if tail := streamMasker.Flush(); len(tail) > 0 {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", replaceModel(tail, alias))
					flusher.Flush()
				}
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
				h.chargeQuota(w, principal.APIKeyID, alias, int64(event.Usage.Total()))
			}
			h.recordUsage(r.Context(), usageRecord{
				started: started, endpoint: "chat.completions", alias: alias, candidate: candidate,
				usage: event.Usage,
				ttft:  firstToken, streaming: true,
				status: http.StatusOK, streamOutcome: "completed", attempts: attempts,
			})
			h.recordMetrics(r.Context(), metricsRecord{
				started: started, endpoint: "chat.completions", alias: alias, candidate: candidate,
				usage:      event.Usage,
				firstToken: firstToken, status: http.StatusOK,
			})
			return
		}
		data := event.Data
		if streamMasker != nil && len(data) > 0 {
			if h.dlpDetector.RejectMode() {
				if hits := streamMasker.Reject(data); len(hits) > 0 {
					h.writeDLPRejectStream(w, r, started, "chat.completions", alias, candidate, firstToken, attempts, body, emittedRunes, hits)
					return
				}
			} else {
				data = streamMasker.Process(data)
			}
		}
		emittedRunes += int64(utf8.RuneCount(data))
		if firstToken.IsZero() {
			firstToken = h.now()
			h.recordUsage(r.Context(), usageRecord{
				started: started, endpoint: "chat.completions", alias: alias, candidate: candidate,
				ttft: firstToken, streaming: true,
				status: http.StatusOK, streamOutcome: "first_token", attempts: attempts,
			})
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", replaceModel(data, alias))
		flusher.Flush()
	}
	if streamMasker != nil {
		if tail := streamMasker.Flush(); len(tail) > 0 {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", replaceModel(tail, alias))
			flusher.Flush()
		}
	}
	if idleFired() || maxFired() {
		h.writeStreamTimeout(w, r, started, "chat.completions", alias, candidate, firstToken, attempts, body, emittedRunes, idleFired, maxFired)
	}
}

// startStreamWatchdogs arms idle and maximum-duration timers for a streaming
// response. idle bounds the gap between consecutive upstream events; max
// bounds the total stream lifetime. Either timer firing cancels the pump
// context. The returned predicates report which timer fired, resetIdle re-arms
// the idle timer after each event, and stop releases both timers.
func startStreamWatchdogs(idle, max time.Duration, cancel context.CancelFunc) (idleFired, maxFired func() bool, resetIdle func(), stop func()) {
	var idleFlag atomic.Bool
	var maxFlag atomic.Bool
	var idleTimer *time.Timer
	var maxTimer *time.Timer
	if idle > 0 {
		idleTimer = time.AfterFunc(idle, func() {
			idleFlag.Store(true)
			cancel()
		})
	}
	if max > 0 {
		maxTimer = time.AfterFunc(max, func() {
			maxFlag.Store(true)
			cancel()
		})
	}
	return func() bool { return idleFlag.Load() },
		func() bool { return maxFlag.Load() },
		func() {
			if idleTimer != nil {
				idleTimer.Reset(idle)
			}
		},
		func() {
			if idleTimer != nil {
				idleTimer.Stop()
			}
			if maxTimer != nil {
				maxTimer.Stop()
			}
		}
}

// chargeStreamEstimate charges a stream that ended without an upstream usage
// chunk: estimated input tokens plus an output-token estimate derived from the
// bytes already emitted to the client. Without this, streams that fail, time
// out, or get aborted mid-generation would bypass quota and cost accounting
// entirely.
func (h handler) chargeStreamEstimate(w http.ResponseWriter, r *http.Request, alias string, body []byte, emittedRunes int64) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal.APIKeyID == "" {
		return
	}
	delta := estimateTokens(body) + runesToTokenEstimate(emittedRunes)
	if delta <= 0 {
		return
	}
	h.chargeQuota(w, principal.APIKeyID, alias, delta)
}

// writeStreamTimeout terminates a stream that exceeded its idle or maximum
// duration limit with an SSE error frame, audit event, and metric. The reason
// is derived from whichever watchdog fired.
func (h handler) writeStreamTimeout(w http.ResponseWriter, r *http.Request, started time.Time, endpoint, alias string, candidate routing.Candidate, firstToken time.Time, attempts retry.Attempts, body []byte, emittedRunes int64, idleFired, maxFired func() bool) {
	h.chargeStreamEstimate(w, r, alias, body, emittedRunes)
	code, message := "upstream_idle_timeout", "upstream stream idle timeout exceeded"
	if maxFired() {
		code, message = "stream_max_duration_exceeded", "stream maximum duration exceeded"
	}
	flusher, _ := w.(http.Flusher)
	if endpoint == "responses" {
		_, _ = fmt.Fprintf(w, "event: error\ndata: {\"error\":{\"message\":%q,\"type\":%q,\"code\":%q}}\n\n", message, code, code)
	} else {
		_, _ = fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":%q,\"code\":%q}}\n\n", message, code, code)
	}
	if flusher != nil {
		flusher.Flush()
	}
	h.logger.Warn("stream terminated by timeout", "request_id", requestID(r), "endpoint", endpoint, "alias", alias, "error_type", code)
	h.recordUsage(r.Context(), usageRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		ttft: firstToken, streaming: true, status: http.StatusBadGateway, errorType: code, streamOutcome: "stream_timeout", attempts: attempts,
	})
	h.recordMetrics(r.Context(), metricsRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		firstToken: firstToken, status: http.StatusBadGateway, errorType: code,
	})
}

// writeDLPRejectNonStreaming rejects a completed response that tripped DLP.
func (h handler) writeDLPRejectNonStreaming(w http.ResponseWriter, r *http.Request, started time.Time, endpoint, alias string, candidate routing.Candidate, hits []dlp.Hit) {
	h.logDLPHits(r.Context(), endpoint, alias, hits, "reject")
	h.recordUsage(r.Context(), usageRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		status: http.StatusBadRequest, errorType: "dlp_rejected",
	})
	h.recordMetrics(r.Context(), metricsRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		status: http.StatusBadRequest, errorType: "dlp_rejected",
	})
	apierr.Write(w, http.StatusBadRequest, "dlp_rejected", "content_policy_error", "Response blocked by data-loss prevention policy")
}

// writeDLPRejectStream ends a stream that tripped DLP with an SSE error frame.
func (h handler) writeDLPRejectStream(w http.ResponseWriter, r *http.Request, started time.Time, endpoint, alias string, candidate routing.Candidate, firstToken time.Time, attempts retry.Attempts, body []byte, emittedRunes int64, hits []dlp.Hit) {
	h.chargeStreamEstimate(w, r, alias, body, emittedRunes)
	h.logDLPHits(r.Context(), endpoint, alias, hits, "reject")
	flusher, _ := w.(http.Flusher)
	if endpoint == "responses" {
		_, _ = fmt.Fprintf(w, "event: error\ndata: {\"error\":{\"message\":%q,\"type\":\"content_policy_error\",\"code\":\"dlp_rejected\"}}\n\n", "Response blocked by data-loss prevention policy")
	} else {
		_, _ = fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":\"content_policy_error\",\"code\":\"dlp_rejected\"}}\n\n", "Response blocked by data-loss prevention policy")
	}
	if flusher != nil {
		flusher.Flush()
	}
	h.recordUsage(r.Context(), usageRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		ttft: firstToken, streaming: true, status: http.StatusBadRequest, errorType: "dlp_rejected", streamOutcome: "dlp_rejected", attempts: attempts,
	})
	h.recordMetrics(r.Context(), metricsRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		firstToken: firstToken, status: http.StatusBadRequest, errorType: "dlp_rejected",
	})
}

// logDLPHits logs the enforcement event and bumps the per-pattern metric.
func (h handler) logDLPHits(ctx context.Context, endpoint, alias string, hits []dlp.Hit, mode string) {
	patterns := dlpHitPatterns(hits)
	h.logger.Warn("dlp enforcement", "endpoint", endpoint, "alias", alias, "mode", mode, "patterns", patterns)
	for _, p := range patterns {
		if h.metrics != nil {
			h.metrics.RecordDLP(ctx, p, mode)
		}
	}
}

// streamOutcomeFor classifies a stream error into (errorType, streamOutcome, status).
// Client cancellation is 499; ProviderError kinds are mapped to their canonical labels;
// everything else is upstream_error/502. Upstream HTTP statuses are sanitised to 502.
func streamOutcomeFor(streamErr error, reqCtx context.Context) (errorType, streamOutcome string, status int) {
	if errors.Is(streamErr, context.Canceled) || errors.Is(reqCtx.Err(), context.Canceled) {
		return "client_aborted", "client_aborted", 499
	}
	var pe *provider.ProviderError
	if errors.As(streamErr, &pe) {
		switch pe.Kind {
		case provider.ErrorKindProtocol:
			return "upstream_protocol_error", "upstream_protocol_error", http.StatusBadGateway
		case provider.ErrorKindTimeout:
			return "upstream_timeout", "upstream_timeout", http.StatusGatewayTimeout
		case provider.ErrorKindCanceled:
			return "client_aborted", "client_aborted", 499
		case provider.ErrorKindNetwork:
			return "upstream_network_error", "upstream_network_error", http.StatusBadGateway
		case provider.ErrorKindUpstream:
			return "upstream_error", "upstream_error", http.StatusBadGateway
		}
	}
	return "upstream_error", "upstream_error", http.StatusBadGateway
}

func (h handler) embeddings(w http.ResponseWriter, r *http.Request) {
	started := h.now()
	body, err := readBody(w, r)
	if err != nil {
		h.writeInvalidRequest(w, r, started, "", routing.Candidate{}, err.Error())
		return
	}
	var request struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.Model) == "" || !validEmbeddingInput(request.Input) {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, "model and a non-empty input are required")
		return
	}
	candidates, err := routing.Resolve(h.config, request.Model)
	if err != nil {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, err.Error())
		return
	}
	adapterRequest := provider.Request{Operation: provider.Embeddings, Body: body}
	candidates, err = h.provider.Filter(candidates, adapterRequest)
	if err != nil {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, err.Error())
		return
	}
	if len(candidates) == 0 {
		h.writeInvalidRequest(w, r, started, request.Model, routing.Candidate{Name: request.Model, Model: request.Model}, "no configured provider supports embeddings")
		return
	}
	recordAccessAlias(r.Context(), request.Model)
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
		if !h.checkAliasQuota(w, r, h.now(), principal, request.Model) {
			return
		}
	}
	result, candidate, attempts, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, h.breaker, candidates, func(ctx context.Context, c routing.Candidate) (provider.Result, error) {
		return h.provider.Do(ctx, adapterRequest, c)
	})
	if err != nil {
		h.writeProviderError(w, r, started, "embeddings", request.Model, candidate, false, err)
		return
	}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal.APIKeyID != "" {
		h.chargeQuota(w, principal.APIKeyID, request.Model, h.chargeableTokens(int64(result.Usage.Total()), body))
	}
	writeRawJSON(w, http.StatusOK, replaceModel(result.Body, request.Model))
	h.recordUsage(r.Context(), usageRecord{
		started: started, endpoint: "embeddings", alias: request.Model, candidate: candidate,
		usage:  result.Usage,
		status: http.StatusOK, attempts: attempts,
	})
	h.recordMetrics(r.Context(), metricsRecord{
		started: started, endpoint: "embeddings", alias: request.Model, candidate: candidate,
		usage: result.Usage, status: http.StatusOK,
	})
}

// writeProviderError converts an upstream failure into a client response and
// audit/metrics entry. Provider RequestError → 400; ProviderError.Kind drives
// the error_type/stream_outcome mapping; everything else falls back to 502.
func (h handler) writeProviderError(w http.ResponseWriter, r *http.Request, started time.Time, endpoint, alias string, candidate routing.Candidate, streaming bool, err error) {
	var requestErr *provider.RequestError
	if errors.As(err, &requestErr) {
		h.recordUsage(r.Context(), usageRecord{
			started: started, endpoint: endpoint, alias: alias, candidate: candidate,
			streaming: streaming, status: http.StatusBadRequest, errorType: "invalid_request",
		})
		h.recordMetrics(r.Context(), metricsRecord{
			started: started, endpoint: endpoint, alias: alias, candidate: candidate,
			status: http.StatusBadRequest, errorType: "invalid_request",
		})
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", requestErr.Error())
		return
	}
	errorType, errorCode, status := classifyProviderError(err)
	h.recordUsage(r.Context(), usageRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		streaming: streaming, status: status, errorType: errorType, attempts: retry.Attempts{},
	})
	h.recordMetrics(r.Context(), metricsRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		status: status, errorType: errorType,
	})
	h.logger.Warn("upstream request failed", "request_id", requestID(r), "endpoint", endpoint, "error", err, "error_type", errorType)
	apierr.Write(w, status, errorCode, "api_error", upstreamErrorMessage)
}

// classifyProviderError maps any error returned from a retry attempt into the
// (errorType, errorCode, httpStatus) tuple used by the gateway's audit/metrics
// and client error envelopes. ProviderError is the preferred signal; legacy
// HTTPError/RequestError are still recognised for backward compatibility.
// Upstream HTTP statuses are always sanitised to 502 so that the gateway never
// surfaces 4xx codes from the upstream to the client.
func classifyProviderError(err error) (errorType, errorCode string, status int) {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		switch pe.Kind {
		case provider.ErrorKindProtocol:
			return "upstream_protocol_error", "upstream_protocol_error", http.StatusBadGateway
		case provider.ErrorKindUpstream:
			return "upstream_error", "upstream_error", http.StatusBadGateway
		case provider.ErrorKindTimeout:
			return "upstream_timeout", "upstream_timeout", http.StatusGatewayTimeout
		case provider.ErrorKindCanceled:
			return "client_aborted", "client_aborted", 499
		case provider.ErrorKindNetwork:
			return "upstream_network_error", "upstream_network_error", http.StatusBadGateway
		case provider.ErrorKindInvalidRequest:
			return "invalid_request", "invalid_request", http.StatusBadRequest
		case provider.ErrorKindUnknown:
			return "upstream_error", "upstream_error", http.StatusBadGateway
		}
	}
	if errors.Is(err, circuitbreaker.ErrOpen) {
		return "upstream_unavailable", "upstream_unavailable", http.StatusBadGateway
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) {
		return "upstream_error", "upstream_error", http.StatusBadGateway
	}
	return "upstream_error", "upstream_error", http.StatusBadGateway
}

func readBody(w http.ResponseWriter, r *http.Request) (json.RawMessage, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func validEmbeddingInput(raw json.RawMessage) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return strings.TrimSpace(one) != ""
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil || len(many) == 0 {
		return false
	}
	for _, item := range many {
		if strings.TrimSpace(item) == "" {
			return false
		}
	}
	return true
}

// replaceModel rewrites the "model" field in the response body using sjson's
// path-aware JSON mutation so the caller's requested alias is reflected back
// regardless of how the upstream reshaped the payload. Invalid JSON falls back
// to the original body.
func replaceModel(body []byte, model string) []byte {
	out, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return body
	}
	return out
}

// chargeableTokens returns the token delta to charge for a request. When the
// upstream reports usage that value is used as-is; otherwise the input body is
// estimated so quotas and cost controls stay enforceable against providers
// that omit usage fields.
func (h handler) chargeableTokens(reported int64, body []byte) int64 {
	if reported > 0 {
		return reported
	}
	return estimateTokens(body)
}

// estimateTokens returns a conservative token estimate for a request body:
// roughly 1 token per 3 UTF-8 runes. It errs toward over-counting for dense
// scripts so quota enforcement never becomes looser than the reported-usage
// path.
func estimateTokens(body []byte) int64 {
	if len(body) == 0 {
		return 1
	}
	est := int64(utf8.RuneCount(body) / 3)
	if est < 1 {
		est = 1
	}
	return est
}

// capRequestTokens clamps the client's max_tokens / max_completion_tokens /
// max_output_tokens to the per-key ceiling when one is configured, so a single
// request can never burn more than its budget even if the upstream honors a
// larger value.
func (h handler) capRequestTokens(principal auth.Principal, body []byte) []byte {
	limits, ok := h.apiKeyLimits[principal.APIKeyID]
	if !ok || limits.MaxTokensPerRequest <= 0 {
		return body
	}
	return capRequestBody(body, limits.MaxTokensPerRequest)
}

// capRequestBody rewrites the request body so each token-ceiling field never
// exceeds cap. Fields absent from the body are left untouched.
func capRequestBody(body []byte, cap int64) []byte {
	out := body
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		var parsed map[string]json.RawMessage
		if err := json.Unmarshal(out, &parsed); err != nil {
			return out
		}
		raw, ok := parsed[field]
		if !ok || len(raw) == 0 {
			continue
		}
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil || v <= 0 || v <= cap {
			continue
		}
		modified, err := sjson.SetBytes(out, field, cap)
		if err != nil {
			continue
		}
		out = modified
	}
	return out
}

// runesToTokenEstimate converts accumulated event-payload runes into an
// output-token estimate (~1 token per 3 runes), used when a stream ends
// before the upstream delivers a usage chunk.
func runesToTokenEstimate(runes int64) int64 {
	if runes <= 0 {
		return 0
	}
	est := runes / 3
	if est < 1 {
		est = 1
	}
	return est
}

// idempotencyEnabled reports whether the Idempotency-Key replay cache is on.
func (h handler) idempotencyEnabled() bool { return h.config.Server.IdempotencyEnabled }

// idemTTL returns the replay-cache TTL, defaulting to one hour for servers
// constructed without the config loader's defaults.
func (h handler) idemTTL() time.Duration {
	if h.config.Server.IdempotencyTTL > 0 {
		return h.config.Server.IdempotencyTTL
	}
	return time.Hour
}

// idemCacheKey returns the cache key for a request: the Idempotency-Key header
// namespaced by the authenticated API key so one tenant can never replay
// another's response. Empty when the header is absent.
func (h handler) idemCacheKey(r *http.Request) string {
	base := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if base == "" {
		return ""
	}
	keyID := ""
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
		keyID = principal.APIKeyID
	}
	return keyID + ":" + base
}

// replayIdempotent returns true after writing the cached response when the
// request carries an Idempotency-Key that already produced a successful
// response within the TTL. Replays are not re-executed and are not re-charged,
// so retries from clients or proxies never double-bill.
func (h handler) replayIdempotent(w http.ResponseWriter, r *http.Request, endpoint, alias string, started time.Time) bool {
	if !h.idempotencyEnabled() {
		return false
	}
	key := h.idemCacheKey(r)
	if key == "" {
		return false
	}
	h.idemMu.Lock()
	entry, ok := h.idem[key]
	h.idemMu.Unlock()
	if !ok {
		return false
	}
	if time.Since(entry.storedAt) > h.idemTTL() {
		h.idemMu.Lock()
		delete(h.idem, key)
		h.idemMu.Unlock()
		return false
	}
	writeRawJSON(w, entry.status, entry.body)
	h.recordUsage(r.Context(), usageRecord{
		started: started, endpoint: endpoint, alias: alias,
		status: entry.status, errorType: "idempotent_replay",
	})
	h.logger.Info("idempotent replay served", "request_id", requestID(r), "endpoint", endpoint, "alias", alias)
	return true
}

// storeIdempotent caches a successful non-streaming response for replay.
// The cache is bounded (4096 entries); expired entries are evicted first and
// the oldest survivor is dropped when the cache is still full.
func (h handler) storeIdempotent(r *http.Request, status int, body []byte) {
	if !h.idempotencyEnabled() {
		return
	}
	key := h.idemCacheKey(r)
	if key == "" {
		return
	}
	h.idemMu.Lock()
	defer h.idemMu.Unlock()
	if len(h.idem) >= 4096 {
		oldestKey, oldestAt := "", time.Now()
		for k, e := range h.idem {
			if time.Since(e.storedAt) > h.idemTTL() {
				delete(h.idem, k)
				continue
			}
			if e.storedAt.Before(oldestAt) {
				oldestAt, oldestKey = e.storedAt, k
			}
		}
		if len(h.idem) >= 4096 && oldestKey != "" {
			delete(h.idem, oldestKey)
		}
	}
	h.idem[key] = idemEntry{status: status, body: append([]byte(nil), body...), storedAt: time.Now()}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeRawJSON(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func requestID(r *http.Request) string {
	if id := middleware.GetReqID(r.Context()); id != "" {
		return id
	}
	return "unknown"
}

func (h handler) recordMetrics(ctx context.Context, rec metricsRecord) {
	principal, _ := auth.PrincipalFromContext(ctx)
	h.metrics.Record(ctx,
		metrics.Request{
			Operation:     rec.endpoint,
			Provider:      rec.candidate.Name,
			Model:         rec.alias,
			UpstreamModel: rec.candidate.Model,
			APIKeyID:      principal.APIKeyID,
			TeamID:        principal.TeamID,
			StartedAt:     rec.started,
		},
		metrics.Result{
			StatusCode:          rec.status,
			ErrorType:           rec.errorType,
			ResponseModel:       rec.candidate.Model,
			InputTokens:         rec.usage.InputTokens,
			OutputTokens:        rec.usage.OutputTokens,
			CacheReadTokens:     rec.usage.CacheReadTokens,
			CacheCreationTokens: rec.usage.CacheCreationTokens,
			FirstTokenAt:        rec.firstToken,
			CompletedAt:         h.now(),
		},
	)
}

func (h handler) recordUsage(ctx context.Context, rec usageRecord) {
	completed := h.now()
	principal, _ := auth.PrincipalFromContext(ctx)
	total := rec.usage.Total()
	event := usage.Event{
		EventID:             usage.NewEventID(),
		RequestID:           middleware.GetReqID(ctx),
		APIKeyID:            principal.APIKeyID,
		TeamID:              principal.TeamID,
		Endpoint:            rec.endpoint,
		Alias:               rec.alias,
		RequestedModel:      rec.alias,
		ResolvedModel:       rec.candidate.Model,
		Provider:            rec.candidate.Name,
		UpstreamModel:       rec.candidate.Model,
		ErrorType:           rec.errorType,
		StreamOutcome:       rec.streamOutcome,
		StatusCode:          rec.status,
		Success:             rec.status >= 200 && rec.status < 300,
		Streaming:           rec.streaming,
		AttemptCount:        rec.attempts.Total,
		RetryCount:          rec.attempts.Retries,
		FailoverCount:       rec.attempts.Failovers,
		InputTokens:         rec.usage.InputTokens,
		OutputTokens:        rec.usage.OutputTokens,
		TotalTokens:         total,
		CacheReadTokens:     rec.usage.CacheReadTokens,
		CacheCreationTokens: rec.usage.CacheCreationTokens,
		ReasoningTokens:     rec.usage.ReasoningTokens,
		DurationMS:          completed.Sub(rec.started).Milliseconds(),
		StartedAt:           rec.started,
		CompletedAt:         completed,
		ClientIP:            clientIP(ctx),
		UserAgent:           userAgent(ctx),
	}
	if !rec.ttft.IsZero() {
		event.TimeToFirstTokenMS = rec.ttft.Sub(rec.started).Milliseconds()
		if event.TimeToFirstTokenMS < 0 {
			event.TimeToFirstTokenMS = 0
		}
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := h.usageSink.Record(recordCtx, event); err != nil {
		h.logger.Warn("usage sink failed", "endpoint", rec.endpoint, "error", err)
	}
}
