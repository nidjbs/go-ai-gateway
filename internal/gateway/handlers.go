package gateway

import (
	"bufio"
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
	"example.com/light-llm-gateway/internal/provider"
	"example.com/light-llm-gateway/internal/retry"
	"example.com/light-llm-gateway/internal/routing"
	"example.com/light-llm-gateway/internal/usage"
)

const maxRequestBodyBytes = 1 << 20

type handler struct {
	config        *config.Config
	logger        *slog.Logger
	authenticator auth.Authenticator
	usageSink     usage.Sink
	provider      *provider.Client
}

func (h handler) healthz(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func (h handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := h.authenticator.Authenticate(r.Context(), r); err != nil {
			if errors.Is(err, auth.ErrUnauthorized) {
				apierr.Write(w, http.StatusUnauthorized, "invalid_api_key", "invalid_request_error", "Invalid API key")
				return
			}
			apierr.Write(w, http.StatusInternalServerError, "authentication_error", "server_error", "Authentication failed")
			return
		}
		next.ServeHTTP(w, r)
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
	Model    string                     `json:"model"`
	Messages json.RawMessage            `json:"messages"`
	Stream   bool                       `json:"stream"`
	Extra    map[string]json.RawMessage `json:"-"`
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
	started := time.Now()
	result, candidate, attempts, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, candidates, func(ctx context.Context, c routing.Candidate) (provider.Result, error) {
		return h.provider.Request(ctx, "chat/completions", bodyWithModel(body, c.Model), c)
	})
	if err != nil {
		h.recordUsage(r.Context(), started, "chat.completions", request.Model, candidate, 0, 0, false, http.StatusBadGateway)
		apierr.Write(w, http.StatusBadGateway, "upstream_error", "api_error", err.Error())
		return
	}
	out := replaceModel(result.Body, request.Model)
	writeRawJSON(w, http.StatusOK, out)
	h.recordUsage(r.Context(), started, "chat.completions", request.Model, candidate, result.InputTokens, result.OutputTokens, false, http.StatusOK)
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
	return body, request, candidates, true
}

func (h handler) chatStream(w http.ResponseWriter, r *http.Request, body json.RawMessage, alias string, candidates []routing.Candidate) {
	started := time.Now()
	stream, candidate, _, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, candidates, func(ctx context.Context, c routing.Candidate) (*provider.Stream, error) {
		return h.provider.OpenStream(ctx, bodyWithModel(body, c.Model), c)
	})
	if err != nil {
		apierr.Write(w, http.StatusBadGateway, "upstream_error", "api_error", err.Error())
		return
	}
	defer stream.Response.Body.Close()
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
	scanner := bufio.NewScanner(stream.Response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			h.recordUsage(r.Context(), started, "chat.completions", alias, candidate, 0, 0, true, http.StatusOK)
			return
		}
		seen = true
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(replaceModel([]byte(payload), alias)))
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil && seen {
		_, _ = fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":\"upstream_error\",\"code\":\"upstream_error\"}}\n\n", err.Error())
		flusher.Flush()
	}
	h.recordUsage(r.Context(), started, "chat.completions", alias, candidate, 0, 0, true, http.StatusBadGateway)
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
	started := time.Now()
	result, candidate, _, err := retry.Execute(r.Context(), h.config.Retry, h.config.Failover, candidates, func(ctx context.Context, c routing.Candidate) (provider.Result, error) {
		return h.provider.Request(ctx, "embeddings", bodyWithModel(body, c.Model), c)
	})
	if err != nil {
		apierr.Write(w, http.StatusBadGateway, "upstream_error", "api_error", err.Error())
		return
	}
	writeRawJSON(w, http.StatusOK, replaceModel(result.Body, request.Model))
	h.recordUsage(r.Context(), started, "embeddings", request.Model, candidate, result.InputTokens, 0, false, http.StatusOK)
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
func bodyWithModel(body json.RawMessage, model string) json.RawMessage {
	var value map[string]json.RawMessage
	if json.Unmarshal(body, &value) != nil {
		return body
	}
	encoded, _ := json.Marshal(model)
	value["model"] = encoded
	out, _ := json.Marshal(value)
	return out
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

func (h handler) recordUsage(ctx context.Context, started time.Time, endpoint, alias string, candidate routing.Candidate, input, output int, streaming bool, status int) {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := h.usageSink.Record(recordCtx, usage.Event{RequestID: middleware.GetReqID(ctx), Endpoint: endpoint, Alias: alias, Provider: candidate.Name, UpstreamModel: candidate.Model, StatusCode: status, StartedAt: started, CompletedAt: time.Now(), InputTokens: input, OutputTokens: output, Streaming: streaming}); err != nil {
		h.logger.Warn("usage sink failed", "endpoint", endpoint, "error", err)
	}
}
