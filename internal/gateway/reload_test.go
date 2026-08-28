package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nidjbs/go-ai-gateway/internal/auth"
	"github.com/nidjbs/go-ai-gateway/internal/config"
)

const testAdminTokenEnv = "GW_TEST_ADMIN_TOKEN"

// writeReloadConfig writes a minimal valid config file with one provider and
// one alias (chat); aliases/topLevel let a test add blocks for the reload.
func writeReloadConfig(t *testing.T, path, providerURL, aliases, topLevel string) {
	t.Helper()
	yaml := "listen: 127.0.0.1:0\n" +
		"auth:\n  mode: none\n" +
		"admin:\n  enabled: true\n  token_env: " + testAdminTokenEnv + "\n" +
		"providers:\n  p1:\n    type: openai\n    base_url: " + providerURL + "/v1\n" +
		"aliases:\n  chat:\n    provider: p1\n    model: m1\n" +
		aliases + topLevel
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// reloadServer loads configPath and builds a gateway wired to it, so reload
// picks the file up through the same config loader the binary uses.
func reloadServer(t *testing.T, configPath string) *Server {
	t.Helper()
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Deps{Config: cfg, ConfigPath: configPath, Logger: testLogger(t), Authenticator: auth.NoopAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func doReload(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	server.Ops.ServeHTTP(rec, req)
	return rec
}

// chatStatus posts a chat completion for model through the main router.
func chatStatus(server *Server, model string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	server.HTTP.Handler.ServeHTTP(rec, req)
	return rec.Code
}

func upstreamChatHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chat-1","object":"chat.completion","model":"m2","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}
}

func TestReloadAppliesNewAlias(t *testing.T) {
	t.Setenv(testAdminTokenEnv, "test-token")
	upstream := httptest.NewServer(upstreamChatHandler())
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "config.yaml")
	writeReloadConfig(t, path, upstream.URL, "", "")
	server := reloadServer(t, path)

	if code := chatStatus(server, "trans"); code != http.StatusBadRequest {
		t.Fatalf("trans before reload = %d, want 400", code)
	}
	// Add a trans alias to the file and reload; the route must switch without restart.
	writeReloadConfig(t, path, upstream.URL, "  trans:\n    provider: p1\n    model: m2\n", "")
	if rec := doReload(t, server, "{}"); rec.Code != http.StatusOK {
		t.Fatalf("reload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if code := chatStatus(server, "trans"); code != http.StatusOK {
		t.Fatalf("trans after reload = %d, want 200", code)
	}
}

func TestReloadKeepsOldConfigOnInvalid(t *testing.T) {
	t.Setenv(testAdminTokenEnv, "test-token")
	upstream := httptest.NewServer(upstreamChatHandler())
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "config.yaml")
	writeReloadConfig(t, path, upstream.URL, "", "")
	server := reloadServer(t, path)

	// Broken YAML must be rejected and leave the running config untouched.
	if err := os.WriteFile(path, []byte("listen: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := doReload(t, server, "{}"); rec.Code != http.StatusBadRequest {
		t.Fatalf("reload status = %d, want 400", rec.Code)
	}
	if code := chatStatus(server, "chat"); code != http.StatusOK {
		t.Fatalf("old config must keep serving, got %d", code)
	}
}

func TestReloadRejectsDriverChange(t *testing.T) {
	t.Setenv(testAdminTokenEnv, "test-token")
	upstream := httptest.NewServer(upstreamChatHandler())
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "config.yaml")
	writeReloadConfig(t, path, upstream.URL, "", "")
	server := reloadServer(t, path)

	// Switching the rate-limit driver would drop live counters: reject 409.
	writeReloadConfig(t, path, upstream.URL, "", "rate_limit:\n  driver: redis\n  options:\n    addr: localhost:6379\n")
	if rec := doReload(t, server, "{}"); rec.Code != http.StatusConflict {
		t.Fatalf("reload status = %d, want 409", rec.Code)
	}
	if code := chatStatus(server, "chat"); code != http.StatusOK {
		t.Fatalf("old config must keep serving, got %d", code)
	}
}

func TestReloadRejectsInvalidRuntimeConfig(t *testing.T) {
	t.Setenv(testAdminTokenEnv, "test-token")
	upstream := httptest.NewServer(upstreamChatHandler())
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "config.yaml")
	writeReloadConfig(t, path, upstream.URL, "", "")
	server := reloadServer(t, path)

	// Valid YAML but an invalid provider (missing base_url) must also be rejected.
	broken := "listen: 127.0.0.1:0\n" +
		"auth:\n  mode: none\n" +
		"providers:\n  p1:\n    type: openai\n" +
		"aliases:\n  chat:\n    provider: p1\n    model: m1\n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := doReload(t, server, "{}"); rec.Code != http.StatusBadRequest {
		t.Fatalf("reload status = %d, want 400", rec.Code)
	}
}
