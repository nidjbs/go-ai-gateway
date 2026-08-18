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
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"example.com/light-llm-gateway/internal/apierr"
	"example.com/light-llm-gateway/internal/auth"
	"example.com/light-llm-gateway/internal/circuitbreaker"
	"example.com/light-llm-gateway/internal/concurrency"
	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/metrics"
	"example.com/light-llm-gateway/internal/provider"
	"example.com/light-llm-gateway/internal/ratelimit"
	"example.com/light-llm-gateway/internal/retry"
	"example.com/light-llm-gateway/internal/routing"
	"example.com/light-llm-gateway/internal/usage"
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
}

// usageRecord is the structured payload every handler passes to recordUsage.
// Carrying it as a value keeps call sites readable when many of these fields
// are zero and avoids the long positional-parameter signature that grew with
// the schema.
type usageRecord struct {
	started       time.Time
	endpoint      string
	alias         string
	candidate     routing.Candidate
	input         int
	output        int
	ttft          time.Time
	streaming     bool
	status        int
	errorType     string
	streamOutcome string
	attempts      retry.Attempts
}

// metricsRecord mirrors usageRecord but carries only the fields the metrics
// recorder consumes — they share most data but not all.
type metricsRecord struct {
	started    time.Time
	endpoint   string
	alias      string
	candidate  routing.Candidate
	input      int
	output     int
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
		if principal.APIKeyID != "" {
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
		out.Data = append(out.Data, model{ID: name, Object: "model", OwnedBy: "light-llm-gateway"})
	}
	writeJSON(w, http.StatusOK, out)
}

type chatRequest struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

// recordBadRequest centralises the "write a 400, log a validation_error audit
// event" pair that every request-validation failure triggers. Returning
// nothing keeps the call sites readable; the caller has already returned
// false / aborted by the time this is invoked.
func (h handler) recordBadRequest(ctx context.Context, started time.Time, endpoint, alias string, candidate routing.Candidate, message string) {
	h.recordUsage(ctx, usageRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		status: http.StatusBadRequest, errorType: "validation_error",
	})
	_ = message
}

func (h handler) writeInvalidRequest(w http.ResponseWriter, r *http.Request, started time.Time, alias string, candidate routing.Candidate, message string) {
	h.recordBadRequest(r.Context(), started, requestEndpoint(r), alias, candidate, message)
	apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", message)
}

func (h handler) chat(w http.ResponseWriter, r *http.Request) {
	body, request, candidates, ok := h.parseChat(w, r)
	if !ok {
		return
	}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
		if !h.checkAliasQuota(w, r, h.now(), principal, request.Model) {
			return
		}
	}
	if request.Stream {
		h.chatStream(w, r, body, request.Model, candidates)
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
		h.chargeQuota(w, principal.APIKeyID, request.Model, int64(result.InputTokens+result.OutputTokens))
	}
	writeRawJSON(w, http.StatusOK, replaceModel(result.Body, request.Model))
	h.recordUsage(r.Context(), usageRecord{
		started: started, endpoint: "chat.completions", alias: request.Model, candidate: candidate,
		input: result.InputTokens, output: result.OutputTokens,
		status: http.StatusOK, attempts: attempts,
	})
	h.recordMetrics(r.Context(), metricsRecord{
		started: started, endpoint: "chat.completions", alias: request.Model, candidate: candidate,
		input: result.InputTokens, output: result.OutputTokens, status: http.StatusOK,
	})
	h.logger.Info("request complete", "request_id", requestID(r), "endpoint", "chat.completions", "provider", candidate.Name, "attempts", attempts.Total, "status", http.StatusOK, "upstream_duration_ms", time.Since(started).Milliseconds())
}

// chargeQuota records usage against all configured quota dimensions for keyID
// and the requested alias. Daily and monthly quotas are key-level aggregates;
// the alias-level quota, when configured, tracks per-model spend.
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

// checkAliasQuota enforces the per-alias daily quota, if configured, after the
// request body has been parsed. It returns true when the request may proceed
// (no quota configured, or quota remaining); false after writing a 429.
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
	seen := false
	var firstToken time.Time
	// pumpStream runs a reader goroutine that pushes events over a bounded
	// channel. When the client is slow, the channel fills and the reader
	// blocks inside stream.Next() — applying real TCP-level backpressure
	// to the upstream socket rather than buffering in user space.
	for result := range pumpStream(r.Context(), stream) {
		if result.err != nil {
			err := result.err
			if err != io.EOF && seen && !errors.Is(err, context.Canceled) {
				_, _ = fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":\"upstream_error\",\"code\":\"upstream_error\"}}\n\n", upstreamErrorMessage)
				flusher.Flush()
			}
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
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
				h.chargeQuota(w, principal.APIKeyID, alias, int64(event.InputTokens+event.OutputTokens))
			}
			h.recordUsage(r.Context(), usageRecord{
				started: started, endpoint: "chat.completions", alias: alias, candidate: candidate,
				input: event.InputTokens, output: event.OutputTokens,
				ttft: firstToken, streaming: true,
				status: http.StatusOK, streamOutcome: "completed", attempts: attempts,
			})
			h.recordMetrics(r.Context(), metricsRecord{
				started: started, endpoint: "chat.completions", alias: alias, candidate: candidate,
				input: event.InputTokens, output: event.OutputTokens,
				firstToken: firstToken, status: http.StatusOK,
			})
			return
		}
		if firstToken.IsZero() {
			firstToken = h.now()
			h.recordUsage(r.Context(), usageRecord{
				started: started, endpoint: "chat.completions", alias: alias, candidate: candidate,
				ttft: firstToken, streaming: true,
				status: http.StatusOK, streamOutcome: "first_token", attempts: attempts,
			})
		}
		seen = true
		_, _ = fmt.Fprintf(w, "data: %s\n\n", replaceModel(event.Data, alias))
		flusher.Flush()
	}
}

// streamOutcomeFor classifies a stream error into the (errorType, streamOutcome,
// status) tuple used by both the audit and metrics recorders. Client-initiated
// cancellation surfaces as 499 client_aborted; everything else falls through
// as upstream_error / 502.
func streamOutcomeFor(streamErr error, reqCtx context.Context) (errorType, streamOutcome string, status int) {
	if errors.Is(streamErr, context.Canceled) || errors.Is(reqCtx.Err(), context.Canceled) {
		return "client_aborted", "client_aborted", 499
	}
	return "upstream_error", "upstream_error", http.StatusBadGateway
}

func (h handler) embeddings(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", err.Error())
		return
	}
	var request struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.Model) == "" || !validEmbeddingInput(request.Input) {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", "model and a non-empty input are required")
		return
	}
	candidates, err := routing.Resolve(h.config, request.Model)
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", err.Error())
		return
	}
	adapterRequest := provider.Request{Operation: provider.Embeddings, Body: body}
	candidates, err = h.provider.Filter(candidates, adapterRequest)
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", err.Error())
		return
	}
	if len(candidates) == 0 {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", "no configured provider supports embeddings")
		return
	}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
		if !h.checkAliasQuota(w, r, h.now(), principal, request.Model) {
			return
		}
	}
	started := h.now()
	result, candidate, attempts, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, h.breaker, candidates, func(ctx context.Context, c routing.Candidate) (provider.Result, error) {
		return h.provider.Do(ctx, adapterRequest, c)
	})
	if err != nil {
		h.writeProviderError(w, r, started, "embeddings", request.Model, candidate, false, err)
		return
	}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal.APIKeyID != "" {
		h.chargeQuota(w, principal.APIKeyID, request.Model, int64(result.InputTokens+result.OutputTokens))
	}
	writeRawJSON(w, http.StatusOK, replaceModel(result.Body, request.Model))
	h.recordUsage(r.Context(), usageRecord{
		started: started, endpoint: "embeddings", alias: request.Model, candidate: candidate,
		input: result.InputTokens, output: result.OutputTokens,
		status: http.StatusOK, attempts: attempts,
	})
	h.recordMetrics(r.Context(), metricsRecord{
		started: started, endpoint: "embeddings", alias: request.Model, candidate: candidate,
		input: result.InputTokens, output: result.OutputTokens, status: http.StatusOK,
	})
}

// writeProviderError converts an upstream-side failure into the appropriate
// client response and audit/metrics entry. Local validation failures from
// upstream adapters are passed through as 400; everything else becomes a
// sanitised 502 (or, if the breaker tripped, an "upstream_unavailable" 502).
func (h handler) writeProviderError(w http.ResponseWriter, r *http.Request, started time.Time, endpoint, alias string, candidate routing.Candidate, streaming bool, err error) {
	var requestErr *provider.RequestError
	if errors.As(err, &requestErr) {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", requestErr.Error())
		return
	}
	errorType := "upstream_error"
	errorCode := "upstream_error"
	status := http.StatusBadGateway
	if errors.Is(err, circuitbreaker.ErrOpen) {
		errorType = "upstream_unavailable"
		errorCode = "upstream_unavailable"
	}
	h.recordUsage(r.Context(), usageRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		streaming: streaming, status: status, errorType: errorType, attempts: retry.Attempts{},
	})
	h.recordMetrics(r.Context(), metricsRecord{
		started: started, endpoint: endpoint, alias: alias, candidate: candidate,
		status: status, errorType: errorType,
	})
	h.logger.Warn("upstream request failed", "request_id", requestID(r), "endpoint", endpoint, "error", err)
	apierr.Write(w, status, errorCode, "api_error", upstreamErrorMessage)
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

func replaceModel(body []byte, model string) []byte {
	var value map[string]json.RawMessage
	if json.Unmarshal(body, &value) != nil {
		return body
	}
	encoded, _ := json.Marshal(model)
	value["model"] = encoded
	out, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return out
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
			StatusCode:    rec.status,
			ErrorType:     rec.errorType,
			ResponseModel: rec.candidate.Model,
			InputTokens:   rec.input,
			OutputTokens:  rec.output,
			FirstTokenAt:  rec.firstToken,
			CompletedAt:   h.now(),
		},
	)
}

func (h handler) recordUsage(ctx context.Context, rec usageRecord) {
	completed := h.now()
	principal, _ := auth.PrincipalFromContext(ctx)
	event := usage.Event{
		EventID:        usage.NewEventID(),
		RequestID:      middleware.GetReqID(ctx),
		APIKeyID:       principal.APIKeyID,
		TeamID:         principal.TeamID,
		Endpoint:       rec.endpoint,
		Alias:          rec.alias,
		RequestedModel: rec.alias,
		ResolvedModel:  rec.candidate.Model,
		Provider:       rec.candidate.Name,
		UpstreamModel:  rec.candidate.Model,
		ErrorType:      rec.errorType,
		StreamOutcome:  rec.streamOutcome,
		StatusCode:     rec.status,
		Success:        rec.status >= 200 && rec.status < 300,
		Streaming:      rec.streaming,
		AttemptCount:   rec.attempts.Total,
		RetryCount:     rec.attempts.Retries,
		FailoverCount:  rec.attempts.Failovers,
		InputTokens:    rec.input,
		OutputTokens:   rec.output,
		TotalTokens:    rec.input + rec.output,
		DurationMS:     completed.Sub(rec.started).Milliseconds(),
		StartedAt:      rec.started,
		CompletedAt:    completed,
		ClientIP:       clientIP(ctx),
		UserAgent:      userAgent(ctx),
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
