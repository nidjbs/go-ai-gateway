package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/nidjbs/go-ai-gateway/internal/auth"
	"github.com/nidjbs/go-ai-gateway/internal/config"
	"github.com/nidjbs/go-ai-gateway/internal/ratelimit"
)

// ── token estimation ──────────────────────────────────────────────────────

func TestEstimateTokensNeverZero(t *testing.T) {
	if got := estimateTokens(nil); got < 1 {
		t.Fatalf("estimateTokens(nil) = %d", got)
	}
	if got := estimateTokens([]byte(`{}`)); got < 1 {
		t.Fatalf("estimateTokens({}) = %d", got)
	}
	if got := estimateTokens([]byte(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`)); got < 1 {
		t.Fatalf("estimateTokens(body) = %d", got)
	}
}

func TestChargeableTokensPrefersReportedUsage(t *testing.T) {
	h := handler{}
	if got := h.chargeableTokens(42, nil); got != 42 {
		t.Fatalf("reported usage must win, got %d", got)
	}
	if got := h.chargeableTokens(0, []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)); got < 1 {
		t.Fatalf("estimated fallback must be positive, got %d", got)
	}
}

func TestCapRequestBodyClampsAllTokenCeilings(t *testing.T) {
	body := []byte(`{"model":"chat","max_tokens":100,"max_completion_tokens":200,"max_output_tokens":300,"messages":[{"role":"user","content":"hi"}]}`)
	out := capRequestBody(body, 10)
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		var v int64
		if err := json.Unmarshal(parsed[field], &v); err != nil {
			t.Fatalf("%s = %s", field, parsed[field])
		}
		if v != 10 {
			t.Fatalf("%s = %d, want 10", field, v)
		}
	}
}

func TestCapRequestBodyLeavesAbsentFieldsAlone(t *testing.T) {
	body := []byte(`{"model":"chat","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	out := capRequestBody(body, 100)
	if string(out) != string(body) {
		t.Fatalf("body must be untouched when under cap: %s", out)
	}
}

// ── per-key request counter ───────────────────────────────────────────────

func TestRequestCounterQuotaEnforced(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chat-1","object":"chat.completion","model":"provider-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	const apiKey = "sk-req-counter"
	server := newAPIKeyTestServer(t, upstream.URL, apiKey, config.KeyLimits{MaxRequestsPerDay: 2})

	statuses := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		rec := httptest.NewRecorder()
		server.HTTP.Handler.ServeHTTP(rec, req)
		statuses = append(statuses, rec.Code)
	}
	if statuses[0] != http.StatusOK || statuses[1] != http.StatusOK {
		t.Fatalf("first two requests should pass, got %v", statuses)
	}
	if statuses[2] != http.StatusTooManyRequests {
		t.Fatalf("third request should be 429, got %v", statuses)
	}
}

// ── per-request token ceiling ─────────────────────────────────────────────

func TestMaxTokensPerRequestCappedAtUpstream(t *testing.T) {
	var received atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		received.Store(string(data))
		_, _ = w.Write([]byte(`{"id":"chat-1","object":"chat.completion","model":"provider-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	const apiKey = "sk-cap"
	server := newAPIKeyTestServer(t, upstream.URL, apiKey, config.KeyLimits{MaxTokensPerRequest: 10})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sent, _ := received.Load().(string)
	if !strings.Contains(sent, `"max_tokens":10`) {
		t.Fatalf("upstream must see capped max_tokens, got: %s", sent)
	}
	if strings.Contains(sent, `"max_tokens":100`) {
		t.Fatalf("upstream must not see the client's larger max_tokens, got: %s", sent)
	}
}

// ── idempotency ───────────────────────────────────────────────────────────

func TestIdempotencyReplaysCachedResponse(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		_, _ = w.Write([]byte(`{"id":"chat-1","object":"chat.completion","model":"provider-model","choices":[{"index":0,"message":{"role":"assistant","content":"replay me"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	const apiKey = "sk-idem"
	server := newAPIKeyTestServer(t, upstream.URL, apiKey, config.KeyLimits{PredayTokens: 100000})
	server.rt.Load().config.Server.IdempotencyEnabled = true

	call := func() string {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Idempotency-Key", "same-request-1")
		rec := httptest.NewRecorder()
		server.HTTP.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	first := call()
	second := call()
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (retry must replay, not re-execute)", upstreamHits.Load())
	}
	if first != second {
		t.Fatalf("replay body differs:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// ── stream failure charging + idle timeout ────────────────────────────────

func TestStreamErrorChargesEstimatedQuota(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Truncate: close without any event and without [DONE].
	}))
	defer upstream.Close()

	const apiKey = "sk-stream-charge"
	limits := config.KeyLimits{PredayTokens: 100000}
	server := newAPIKeyTestServer(t, upstream.URL, apiKey, limits)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)

	status := server.quotaStore.Peek(ratelimit.QuotaScope{KeyID: apiKey, Window: ratelimit.WindowDaily}, limits.PredayTokens, time.Now())
	if status.Used <= 0 {
		t.Fatalf("aborted stream must still charge an estimated delta, used = %d", status.Used)
	}
}

func TestStreamIdleTimeoutWritesErrorFrame(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(400 * time.Millisecond) // outlive the gateway's idle window
	}))
	defer upstream.Close()

	cfg := config.Config{
		Listen:  "127.0.0.1:0",
		Healthz: "127.0.0.1:0",
		Providers: map[string]config.Provider{
			"local": {BaseURL: upstream.URL, APIKey: "upstream-token"},
		},
		Aliases:  map[string]config.Alias{"chat": {Provider: "local", Model: "provider-model"}},
		Retry:    config.RetryConfig{MaxAttemptsPerProvider: 1},
		Failover: config.FailoverConfig{},
		Server:   config.ServerConfig{StreamIdleTimeout: 100 * time.Millisecond},
	}
	server, err := New(Deps{Config: &cfg, Logger: testLogger(t), Authenticator: auth.NoopAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "upstream_idle_timeout") {
		t.Fatalf("idle-stalled stream must end with a timeout error frame, got: %q", body)
	}
}

// ── readiness probes ──────────────────────────────────────────────────────

func TestReadinessProbeTracksRedisDependency(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	redisServer := miniredis.NewMiniRedis()
	if err := redisServer.Start(); err != nil {
		t.Fatal(err)
	}
	defer redisServer.Close()

	cfg := &config.Config{
		Listen:         "127.0.0.1:0",
		Healthz:        "127.0.0.1:0",
		ReadyzWaitTime: 0,
		Providers:      map[string]config.Provider{"local": {BaseURL: upstream.URL, APIKey: "t"}},
		Aliases:        map[string]config.Alias{"chat": {Provider: "local", Model: "m"}},
		Retry:          config.RetryConfig{MaxAttemptsPerProvider: 1},
		Failover:       config.FailoverConfig{},
		RateLimit:      config.StorageDriver{Driver: "redis", Options: map[string]any{"addr": redisServer.Addr()}},
		Quota:          config.StorageDriver{Driver: "redis", Options: map[string]any{"addr": redisServer.Addr()}},
	}
	server, err := New(Deps{Config: cfg, Logger: testLogger(t), Authenticator: auth.NoopAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.CheckReadiness(); err != nil {
		t.Fatalf("ready with live redis: %v", err)
	}
	if code := readyzCode(server); code != http.StatusNoContent {
		t.Fatalf("readyz = %d, want 204", code)
	}

	// Kill redis; the same replica must now report not-ready.
	redisServer.Close()
	if err := server.CheckReadiness(); err == nil {
		t.Fatal("CheckReadiness must fail after redis is down")
	}
	if code := readyzCode(server); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503 after redis outage", code)
	}
}

// ── ops endpoints: /version + token auth ──────────────────────────────────

func TestOpsVersionEndpoint(t *testing.T) {
	server := newTestServer("http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["version"] == "" || out["commit"] == "" {
		t.Fatalf("version payload incomplete: %s", rec.Body.String())
	}
}

func TestOpsTokenMiddlewareProtectsMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	t.Setenv("OPS_TEST_TOKEN", "ops-secret")
	cfg := &config.Config{
		Listen:      "127.0.0.1:0",
		Healthz:     "127.0.0.1:0",
		OpsTokenEnv: "OPS_TEST_TOKEN",
		Providers:   map[string]config.Provider{"local": {BaseURL: upstream.URL, APIKey: "t"}},
		Aliases:     map[string]config.Alias{"chat": {Provider: "local", Model: "m"}},
		Retry:       config.RetryConfig{MaxAttemptsPerProvider: 1},
		Failover:    config.FailoverConfig{},
	}
	server, err := New(Deps{Config: cfg, Logger: testLogger(t), Authenticator: auth.NoopAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}

	noToken := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, noToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("metrics without token = %d, want 401", rec.Code)
	}

	withToken := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	withToken.Header.Set("Authorization", "Bearer ops-secret")
	rec = httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, withToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics with token = %d, want 200", rec.Code)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

// newAPIKeyTestServer builds a gateway with api-key auth and the given limits.
func newAPIKeyTestServer(t *testing.T, upstreamURL, apiKey string, limits config.KeyLimits) *Server {
	t.Helper()
	cfg := &config.Config{
		Listen:  "127.0.0.1:0",
		Healthz: "127.0.0.1:0",
		Auth:    config.AuthConfig{Mode: "api-key"},
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

func readyzCode(server *Server) int {
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	return rec.Code
}

var _ = bytes.NewReader
