package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/apierr"
	"github.com/nidjbs/go-ai-gateway/internal/usage"
)

// adminTokenMiddleware requires the admin Bearer token (constant-time
// compare), distinct from the ops token.
func adminTokenMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				apierr.Write(w, http.StatusUnauthorized, "invalid_admin_token", "invalid_request_error", "Invalid admin token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type revokeRequest struct {
	KeyID string `json:"key_id"`
}

// revokeKey cuts off an API key at runtime; replicas honor it within the
// lookup cache TTL.
func (h handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	if h.rt().revoker == nil {
		apierr.Write(w, http.StatusNotFound, "admin_disabled", "server_error", "Admin surface is not enabled")
		return
	}
	var req revokeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.KeyID) == "" {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", "key_id is required")
		return
	}
	if err := h.rt().revoker.Revoke(r.Context(), req.KeyID); err != nil {
		h.logger.Error("admin: revoke failed", "key_id", req.KeyID, "error", err)
		apierr.Write(w, http.StatusInternalServerError, "revocation_failed", "server_error", "Failed to revoke key")
		return
	}
	h.logger.Warn("admin: api key revoked", "key_id", req.KeyID, "request_id", requestID(r))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"key_id": req.KeyID, "revoked": true})
}

// listRevoked returns the currently-revoked key IDs.
func (h handler) listRevoked(w http.ResponseWriter, r *http.Request) {
	if h.rt().revoker == nil {
		apierr.Write(w, http.StatusNotFound, "admin_disabled", "server_error", "Admin surface is not enabled")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": h.rt().revoker.RevokedKeys()})
}

// usageSummary aggregates usage by team_id/key_id/alias/from/to. Requires
// a queryable usage sink (sqlite); the audit sink returns 501.
func (h handler) usageSummary(w http.ResponseWriter, r *http.Request) {
	q, ok := h.usageQueryer()
	if !ok {
		apierr.Write(w, http.StatusNotImplemented, "usage_query_unsupported", "server_error", "Usage querying requires a queryable usage driver (e.g. sqlite)")
		return
	}
	filter, err := parseUsageFilter(r, h.now())
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	sum, err := q.Summary(ctx, filter)
	if err != nil {
		h.logger.Error("admin: usage summary failed", "error", err, "request_id", requestID(r))
		apierr.Write(w, http.StatusInternalServerError, "usage_query_failed", "server_error", "Usage query failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sum)
}

// usageSeries returns a per-bucket time series; bucket is hour or day.
func (h handler) usageSeries(w http.ResponseWriter, r *http.Request) {
	q, ok := h.usageQueryer()
	if !ok {
		apierr.Write(w, http.StatusNotImplemented, "usage_query_unsupported", "server_error", "Usage querying requires a queryable usage driver (e.g. sqlite)")
		return
	}
	filter, err := parseUsageFilter(r, h.now())
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", err.Error())
		return
	}
	bucket := 24 * time.Hour
	switch strings.ToLower(r.URL.Query().Get("bucket")) {
	case "hour", "hourly":
		bucket = time.Hour
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	buckets, err := q.Series(ctx, filter, bucket)
	if err != nil {
		h.logger.Error("admin: usage series failed", "error", err, "request_id", requestID(r))
		apierr.Write(w, http.StatusInternalServerError, "usage_query_failed", "server_error", "Usage query failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"bucket": bucket.String(), "buckets": buckets})
}

// usageQueryer returns the sink as a usage.Queryer, or false when it can't.
func (h handler) usageQueryer() (usage.Queryer, bool) {
	q, ok := h.usageSink.(usage.Queryer)
	return q, ok
}

// parseUsageFilter reads team_id/key_id/alias/from/to query parameters
// (RFC3339); invalid or future ranges are rejected.
func parseUsageFilter(r *http.Request, now time.Time) (usage.QueryFilter, error) {
	q := r.URL.Query()
	filter := usage.QueryFilter{
		TeamID:   q.Get("team_id"),
		APIKeyID: q.Get("key_id"),
		Alias:    q.Get("alias"),
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return filter, fmt.Errorf("from must be RFC3339: %w", err)
		}
		filter.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return filter, fmt.Errorf("to must be RFC3339: %w", err)
		}
		filter.To = t
	}
	if !filter.From.IsZero() && !filter.To.IsZero() && !filter.To.After(filter.From) {
		return filter, errors.New("to must be after from")
	}
	if !filter.From.IsZero() && filter.From.After(now) {
		return filter, errors.New("from must not be in the future")
	}
	return filter, nil
}
