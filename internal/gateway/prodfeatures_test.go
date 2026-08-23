package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nidjbs/go-ai-gateway/internal/auth"
	"github.com/nidjbs/go-ai-gateway/internal/config"
	"github.com/nidjbs/go-ai-gateway/internal/usage"
)

// ── admin key revocation ──────────────────────────────────────────────────

func TestAdminRevokeKeyCutsOffAuthentication(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_TOKEN", "admin-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	server := newAdminAPIKeyTestServer(t, upstream.URL, "sk-test", config.KeyLimits{})
	chatReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		rec := httptest.NewRecorder()
		server.HTTP.Handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := chatReq(); rec.Code != http.StatusOK {
		t.Fatalf("pre-revoke status = %d, body = %s", rec.Code, rec.Body.String())
	}

	revokeBody := strings.NewReader(`{"key_id":"sk-test"}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/keys/revoke", revokeBody)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = chatReq()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-revoke status = %d, body = %s (want 401)", rec.Code, rec.Body.String())
	} else if !strings.Contains(rec.Body.String(), "revoked_api_key") {
		t.Fatalf("expected revoked_api_key error, got: %s", rec.Body.String())
	}

	server2 := newAPIKeyTestServer(t, upstream.URL, "sk-other", config.KeyLimits{})
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer sk-other")
	rec2 := httptest.NewRecorder()
	server2.HTTP.Handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("unrevoked key status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
}

func TestAdminTokenRequired(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_TOKEN", "admin-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	server := newAdminAPIKeyTestServer(t, upstream.URL, "sk-test", config.KeyLimits{})

	req := httptest.NewRequest(http.MethodPost, "/admin/keys/revoke", strings.NewReader(`{"key_id":"sk-test"}`))
	rec := httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/keys/revoke", strings.NewReader(`{"key_id":"sk-test"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", rec.Code)
	}
}

func TestAdminRevokedListing(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_TOKEN", "admin-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	server := newAdminAPIKeyTestServer(t, upstream.URL, "sk-test", config.KeyLimits{})

	req := httptest.NewRequest(http.MethodPost, "/admin/keys/revoke", strings.NewReader(`{"key_id":"sk-a"}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/keys/revoked", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec = httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var list struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Keys) != 1 || list.Keys[0] != "sk-a" {
		t.Fatalf("revoked list = %v, want [sk-a]", list.Keys)
	}
}

// newAdminAPIKeyTestServer builds an api-key-authenticated gateway with the
// admin surface enabled; the admin token is read from GATEWAY_ADMIN_TOKEN
// (set with t.Setenv by callers).
func newAdminAPIKeyTestServer(t *testing.T, upstreamURL, apiKey string, limits config.KeyLimits) *Server {
	t.Helper()
	cfg := &config.Config{
		Listen:  "127.0.0.1:0",
		Healthz: "127.0.0.1:0",
		Auth:    config.AuthConfig{Mode: "api-key"},
		Admin:   config.AdminConfig{Enabled: true, TokenEnv: "GATEWAY_ADMIN_TOKEN"},
		Teams: []config.TeamConfig{{
			ID:      "team-test",
			APIKeys: []config.APIKeyConfig{{ID: apiKey, Key: apiKey, Limits: limits}},
		}},
		Providers: map[string]config.Provider{
			"local": {BaseURL: upstreamURL, APIKey: "upstream-token"},
		},
		Aliases:  map[string]config.Alias{"chat": {Provider: "local", Model: "provider-model"}},
		Retry:    config.RetryConfig{MaxAttemptsPerProvider: 1},
		Failover: config.FailoverConfig{},
	}
	apiKeyAuth, err := auth.New(cfg.Auth, cfg.Teams)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Deps{
		Config:        cfg,
		Logger:        testLogger(t),
		Authenticator: apiKeyAuth,
		APIKeyLimits:  map[string]config.KeyLimits{apiKey: limits},
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// ── usage query endpoints ─────────────────────────────────────────────────

func newUsageTestServer(t *testing.T, sink usage.Sink) *Server {
	t.Helper()
	cfg := &config.Config{
		Listen:  "127.0.0.1:0",
		Healthz: "127.0.0.1:0",
		Auth:    config.AuthConfig{Mode: "none"},
		Admin:   config.AdminConfig{Enabled: true, TokenEnv: "GATEWAY_ADMIN_TOKEN"},
		Providers: map[string]config.Provider{
			"local": {BaseURL: "http://127.0.0.1:1", APIKey: "upstream-token"},
		},
		Aliases:  map[string]config.Alias{"chat": {Provider: "local", Model: "provider-model"}},
		Retry:    config.RetryConfig{MaxAttemptsPerProvider: 1},
		Failover: config.FailoverConfig{},
	}
	server, err := New(Deps{Config: cfg, Logger: testLogger(t), Authenticator: auth.NoopAuthenticator{}, UsageSink: sink})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestAdminUsageSummary(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_TOKEN", "admin-secret")
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	sink, err := usage.NewSQLSink(db, usage.SQLiteSchema(), usage.DefaultInsertSQL(usage.DefaultTable, "?"))
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	now := time.Now().UTC()
	_ = sink.Record(context.Background(), usage.Event{APIKeyID: "k1", TeamID: "t1", Alias: "chat", Success: true, InputTokens: 10, OutputTokens: 20, TotalTokens: 30, TotalCostMicros: 5, DurationMS: 100, StartedAt: now.Add(-time.Hour), CompletedAt: now})
	_ = sink.Record(context.Background(), usage.Event{APIKeyID: "k1", TeamID: "t1", Alias: "chat", Success: false, InputTokens: 1, OutputTokens: 0, TotalTokens: 1, TotalCostMicros: 2, DurationMS: 50, StartedAt: now.Add(-2 * time.Hour), CompletedAt: now})

	server := newUsageTestServer(t, sink)
	req := httptest.NewRequest(http.MethodGet, "/admin/usage/summary", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sum usage.Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 2 || sum.Successes != 1 || sum.Failures != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.TotalTokens != 31 || sum.CostMicros != 7 {
		t.Fatalf("summary tokens/cost = %+v", sum)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/usage/summary?team_id=t1", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec = httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered summary status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/usage/summary?from=not-a-time", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec = httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad filter status = %d, want 400", rec.Code)
	}
}

func TestAdminUsageUnsupportedDriver(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_TOKEN", "admin-secret")
	server := newUsageTestServer(t, usage.NewAuditSink(testLogger(t)))
	req := httptest.NewRequest(http.MethodGet, "/admin/usage/summary", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body = %s", rec.Code, rec.Body.String())
	}
}

// ── access log ────────────────────────────────────────────────────────────

func TestAccessLogEmitsStructuredRecord(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Listen:  "127.0.0.1:0",
		Healthz: "127.0.0.1:0",
		Auth:    config.AuthConfig{Mode: "none"},
		Server:  config.ServerConfig{AccessLog: config.AccessLogConfig{Enabled: true, SampleRatio: 1.0}},
		Providers: map[string]config.Provider{
			"local": {BaseURL: upstream.URL, APIKey: "upstream-token"},
		},
		Aliases:  map[string]config.Alias{"chat": {Provider: "local", Model: "provider-model"}},
		Retry:    config.RetryConfig{MaxAttemptsPerProvider: 1},
		Failover: config.FailoverConfig{},
	}
	server, err := New(Deps{Config: cfg, Logger: logger, Authenticator: auth.NoopAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"secret-payload-123"}]}`))
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	out := buf.String()
	if !strings.Contains(out, "msg=access") {
		t.Fatalf("expected access log line, got: %q", out)
	}
	if !strings.Contains(out, "path=/v1/chat/completions") || !strings.Contains(out, "status=200") {
		t.Fatalf("access log missing fields: %q", out)
	}
	if strings.Contains(out, "secret-payload-123") {
		t.Fatalf("access log must never contain request body content: %q", out)
	}
}

func TestAccessLogSamplingExcludesProbes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	cfg := &config.Config{
		Listen:  "127.0.0.1:0",
		Healthz: "127.0.0.1:0",
		Auth:    config.AuthConfig{Mode: "none"},
		Server:  config.ServerConfig{AccessLog: config.AccessLogConfig{Enabled: true, SampleRatio: 1.0}},
		Providers: map[string]config.Provider{
			"local": {BaseURL: upstream.URL, APIKey: "upstream-token"},
		},
		Aliases:  map[string]config.Alias{"chat": {Provider: "local", Model: "provider-model"}},
		Retry:    config.RetryConfig{MaxAttemptsPerProvider: 1},
		Failover: config.FailoverConfig{},
	}
	server, err := New(Deps{Config: cfg, Logger: logger, Authenticator: auth.NoopAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", rec.Code)
	}
	if strings.Contains(buf.String(), "msg=access") {
		t.Fatalf("probe paths must be excluded from access log: %q", buf.String())
	}
}

// ── DLP output protection ─────────────────────────────────────────────────

func newDLPTestServer(t *testing.T, dlpCfg config.DLPConfig, upstreamURL string) *Server {
	t.Helper()
	cfg := &config.Config{
		Listen:  "127.0.0.1:0",
		Healthz: "127.0.0.1:0",
		Auth:    config.AuthConfig{Mode: "none"},
		DLP:     dlpCfg,
		Providers: map[string]config.Provider{
			"local": {BaseURL: upstreamURL, APIKey: "upstream-token"},
		},
		Aliases:  map[string]config.Alias{"chat": {Provider: "local", Model: "provider-model"}},
		Retry:    config.RetryConfig{MaxAttemptsPerProvider: 1},
		Failover: config.FailoverConfig{},
	}
	server, err := New(Deps{Config: cfg, Logger: testLogger(t), Authenticator: auth.NoopAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

const chatCompletionPII = `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"contact alice@example.com at 415-555-0134"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`

func TestDLPMaskNonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(chatCompletionPII))
	}))
	defer upstream.Close()

	server := newDLPTestServer(t, config.DLPConfig{Enabled: true, Mode: "mask"}, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "alice@example.com") || strings.Contains(body, "415-555-0134") {
		t.Fatalf("PII leaked through mask: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("expected mask text, got: %s", body)
	}
}

func TestDLPRejectNonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(chatCompletionPII))
	}))
	defer upstream.Close()

	server := newDLPTestServer(t, config.DLPConfig{Enabled: true, Mode: "reject"}, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s (want 400)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dlp_rejected") {
		t.Fatalf("expected dlp_rejected, got: %s", rec.Body.String())
	}
}

func TestDLPDisabledPassesThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(chatCompletionPII))
	}))
	defer upstream.Close()

	server := newDLPTestServer(t, config.DLPConfig{Enabled: false}, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "alice@example.com") {
		t.Fatalf("disabled DLP must pass content through: %s", rec.Body.String())
	}
}

func TestDLPMaskStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"contact "}}]}`+"\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"alice@example.com now"}}]}`+"\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	server := newDLPTestServer(t, config.DLPConfig{Enabled: true, Mode: "mask"}, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "alice@example.com") {
		t.Fatalf("streamed PII leaked through mask: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("expected masked stream content: %s", body)
	}
}

func TestDLPRejectStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"email is "}}]}`+"\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"alice@example.com"}}]}`+"\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	server := newDLPTestServer(t, config.DLPConfig{Enabled: true, Mode: "reject"}, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "dlp_rejected") {
		t.Fatalf("expected dlp_rejected SSE error frame, got: %s", body)
	}
	if strings.Contains(body, "alice@example.com") {
		t.Fatalf("rejected stream must not leak PII: %s", body)
	}
}

func TestDLPResponsesMaskStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+`data: {"type":"response.output_text.delta","delta":"secret: "}`+"\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+`data: {"type":"response.output_text.delta","delta":"alice@example.com"}`+"\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: response.completed\n"+`data: {"type":"response.completed"}`+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	server := newDLPTestServer(t, config.DLPConfig{Enabled: true, Mode: "mask"}, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"chat","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "alice@example.com") {
		t.Fatalf("responses stream PII leaked: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("expected masked delta: %s", body)
	}
}
