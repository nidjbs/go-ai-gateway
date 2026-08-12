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
	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/metrics"
	"example.com/light-llm-gateway/internal/provider"
	"example.com/light-llm-gateway/internal/ratelimit"
	"example.com/light-llm-gateway/internal/retry"
	"example.com/light-llm-gateway/internal/routing"
	"example.com/light-llm-gateway/internal/usage"
)

const maxRequestBodyBytes = 1 << 20
const upstreamErrorMessage = "upstream request failed"

type handler struct {
	config        *config.Config
	logger        *slog.Logger
	authenticator auth.Authenticator
	usageSink     usage.Sink
	provider      *provider.Client
	metrics       *metrics.Recorder
	limiter       ratelimit.Limiter
	quotaStore    ratelimit.QuotaStore
	now           func() time.Time
	apiKeyLimits  map[string]config.KeyLimits
}

func (h handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := h.authenticator.Authenticate(r.Context(), r)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthorized) {
				apierr.Write(w, http.StatusUnauthorized, "invalid_api_key", "invalid_request_error", "Invalid API key")
				return
			}
			apierr.Write(w, http.StatusInternalServerError, "authentication_error", "server_error", "Authentication failed")
			return
		}
		ctx := auth.ContextWithPrincipal(r.Context(), principal)
		if principal.APIKeyID != "" {
			limits := h.apiKeyLimits[principal.APIKeyID]
			if h.limiter != nil {
				decision := h.limiter.Allow(principal.APIKeyID, limits, h.now())
				writeRateLimitHeaders(w, limits, decision)
				if !decision.Allowed {
					apierr.Write(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_error", "API key rate limit exceeded")
					return
				}
			}
			if h.quotaStore != nil && limits.PredayTokens > 0 {
				status := h.quotaStore.Peek(principal.APIKeyID, limits.PredayTokens, h.now())
				writeQuotaHeaders(w, status)
				if status.Remaining <= 0 {
					apierr.Write(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_error", "API key daily quota exceeded")
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

func (h handler) chat(w http.ResponseWriter, r *http.Request) {
	body, request, candidates, ok := h.parseChat(w, r)
	if !ok {
		return
	}
	if request.Stream {
		h.chatStream(w, r, body, request.Model, candidates)
		return
	}
	started := h.now()
	adapterRequest := provider.Request{Operation: provider.ChatCompletions, Body: body}
	result, candidate, attempts, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, candidates, func(ctx context.Context, c routing.Candidate) (provider.Result, error) {
		return h.provider.Do(ctx, adapterRequest, c)
	})
	if err != nil {
		h.writeProviderError(w, r, started, "chat.completions", request.Model, candidate, false, err)
		return
	}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal.APIKeyID != "" {
		if limits, ok := h.apiKeyLimits[principal.APIKeyID]; ok && limits.PredayTokens > 0 && result.InputTokens+result.OutputTokens > 0 {
			status := h.quotaStore.Charge(principal.APIKeyID, limits.PredayTokens, int64(result.InputTokens+result.OutputTokens), h.now())
			writeQuotaHeaders(w, status)
		}
	}
	writeRawJSON(w, http.StatusOK, replaceModel(result.Body, request.Model))
	h.recordUsage(r.Context(), started, "chat.completions", request.Model, candidate, result.InputTokens, result.OutputTokens, false, http.StatusOK, attempts)
	h.recordMetrics(r.Context(), started, "chat.completions", request.Model, candidate, result.InputTokens, result.OutputTokens, time.Time{}, http.StatusOK, "")
	h.logger.Info("request complete", "request_id", requestID(r), "endpoint", "chat.completions", "provider", candidate.Name, "attempts", attempts.Total, "status", http.StatusOK, "upstream_duration_ms", time.Since(started).Milliseconds())
}

func (h handler) parseChat(w http.ResponseWriter, r *http.Request) (json.RawMessage, chatRequest, []routing.Candidate, bool) {
	var request chatRequest
	body, err := readBody(w, r)
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", err.Error())
		return nil, request, nil, false
	}
	if err := json.Unmarshal(body, &request); err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", err.Error())
		return nil, request, nil, false
	}
	if strings.TrimSpace(request.Model) == "" || len(request.Messages) == 0 || string(request.Messages) == "null" {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", "model and messages are required")
		return nil, request, nil, false
	}
	candidates, err := routing.Resolve(h.config, request.Model)
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", err.Error())
		return nil, request, nil, false
	}
	adapterRequest := provider.Request{Operation: provider.ChatCompletions, Body: body}
	candidates, err = h.provider.Filter(candidates, adapterRequest)
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", err.Error())
		return nil, request, nil, false
	}
	if len(candidates) == 0 {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", "no configured provider supports chat.completions")
		return nil, request, nil, false
	}
	return body, request, candidates, true
}

func (h handler) chatStream(w http.ResponseWriter, r *http.Request, body json.RawMessage, alias string, candidates []routing.Candidate) {
	started := h.now()
	adapterRequest := provider.Request{Operation: provider.ChatCompletions, Body: body}
	stream, candidate, attempts, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, candidates, func(ctx context.Context, c routing.Candidate) (provider.Stream, error) {
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
	for {
		event, err := stream.Next()
		if err != nil {
			if err != io.EOF && seen {
				_, _ = fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":\"upstream_error\",\"code\":\"upstream_error\"}}\n\n", upstreamErrorMessage)
				flusher.Flush()
			}
			h.recordUsage(r.Context(), started, "chat.completions", alias, candidate, 0, 0, true, http.StatusBadGateway, attempts)
			h.recordMetrics(r.Context(), started, "chat.completions", alias, candidate, 0, 0, firstToken, http.StatusBadGateway, "upstream_error")
			return
		}
		if event.Done {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			h.recordUsage(r.Context(), started, "chat.completions", alias, candidate, event.InputTokens, event.OutputTokens, true, http.StatusOK, attempts)
			h.recordMetrics(r.Context(), started, "chat.completions", alias, candidate, event.InputTokens, event.OutputTokens, firstToken, http.StatusOK, "")
			return
		}
		if firstToken.IsZero() {
			firstToken = h.now()
		}
		seen = true
		_, _ = fmt.Fprintf(w, "data: %s\n\n", replaceModel(event.Data, alias))
		flusher.Flush()
	}
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
	started := h.now()
	result, candidate, attempts, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, candidates, func(ctx context.Context, c routing.Candidate) (provider.Result, error) {
		return h.provider.Do(ctx, adapterRequest, c)
	})
	if err != nil {
		h.writeProviderError(w, r, started, "embeddings", request.Model, candidate, false, err)
		return
	}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal.APIKeyID != "" {
		if limits, ok := h.apiKeyLimits[principal.APIKeyID]; ok && limits.PredayTokens > 0 && result.InputTokens+result.OutputTokens > 0 {
			status := h.quotaStore.Charge(principal.APIKeyID, limits.PredayTokens, int64(result.InputTokens+result.OutputTokens), h.now())
			writeQuotaHeaders(w, status)
		}
	}
	writeRawJSON(w, http.StatusOK, replaceModel(result.Body, request.Model))
	h.recordUsage(r.Context(), started, "embeddings", request.Model, candidate, result.InputTokens, result.OutputTokens, false, http.StatusOK, attempts)
	h.recordMetrics(r.Context(), started, "embeddings", request.Model, candidate, result.InputTokens, result.OutputTokens, time.Time{}, http.StatusOK, "")
}

func (h handler) writeProviderError(w http.ResponseWriter, r *http.Request, started time.Time, endpoint, alias string, candidate routing.Candidate, streaming bool, err error) {
	var requestErr *provider.RequestError
	if errors.As(err, &requestErr) {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", requestErr.Error())
		return
	}
	h.recordUsage(r.Context(), started, endpoint, alias, candidate, 0, 0, streaming, http.StatusBadGateway, retry.Attempts{})
	h.recordMetrics(r.Context(), started, endpoint, alias, candidate, 0, 0, time.Time{}, http.StatusBadGateway, "upstream_error")
	h.logger.Warn("upstream request failed", "request_id", requestID(r), "endpoint", endpoint, "error", err)
	apierr.Write(w, http.StatusBadGateway, "upstream_error", "api_error", upstreamErrorMessage)
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

func (h handler) recordMetrics(ctx context.Context, started time.Time, endpoint, alias string, candidate routing.Candidate, input, output int, firstToken time.Time, status int, errorType string) {
	principal, _ := auth.PrincipalFromContext(ctx)
	h.metrics.Record(ctx, metrics.Request{Operation: endpoint, Provider: candidate.Name, Model: alias, UpstreamModel: candidate.Model, APIKeyID: principal.APIKeyID, TeamID: principal.TeamID, StartedAt: started}, metrics.Result{StatusCode: status, ErrorType: errorType, ResponseModel: candidate.Model, InputTokens: input, OutputTokens: output, FirstTokenAt: firstToken, CompletedAt: h.now()})
}

func (h handler) recordUsage(ctx context.Context, started time.Time, endpoint, alias string, candidate routing.Candidate, input, output int, streaming bool, status int, attempts retry.Attempts) {
	completed := h.now()
	principal, _ := auth.PrincipalFromContext(ctx)
	event := usage.Event{RequestID: middleware.GetReqID(ctx), APIKeyID: principal.APIKeyID, TeamID: principal.TeamID, Endpoint: endpoint, Alias: alias, RequestedModel: alias, ResolvedModel: candidate.Model, Provider: candidate.Name, UpstreamModel: candidate.Model, StatusCode: status, Success: status >= 200 && status < 300, InputTokens: input, OutputTokens: output, TotalTokens: input + output, DurationMS: completed.Sub(started).Milliseconds(), StartedAt: started, CompletedAt: completed, Streaming: streaming, AttemptCount: attempts.Total, RetryCount: attempts.Retries, FailoverCount: attempts.Failovers}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := h.usageSink.Record(recordCtx, event); err != nil {
		h.logger.Warn("usage sink failed", "endpoint", endpoint, "error", err)
	}
}
