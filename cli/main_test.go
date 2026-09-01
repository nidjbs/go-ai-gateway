package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeTestCLIConfig writes a temporary CLI config pointing at srv.
func writeTestCLIConfig(t *testing.T, gatewayURL, extra string) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	data := "gateway_url: " + gatewayURL + "\napi_key: sk-test\n" + extra
	if err := os.WriteFile(cfgPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GW_CONFIG", cfgPath)
}

func TestRunTransEndToEnd(t *testing.T) {
	srv := mockGateway(t)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: chat\n")

	if code := run([]string{"trans", "hello"}); code != 0 {
		t.Fatalf("run(trans) = %d", code)
	}
}

func TestRunModels(t *testing.T) {
	srv := mockGateway(t)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "")

	if code := run([]string{"models"}); code != 0 {
		t.Fatalf("run(models) = %d", code)
	}
}

func TestModelFlagSelectsAlias(t *testing.T) {
	var gotModel string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeTestCLIConfig(t, srv.URL, "default_alias: default-alias\n")
	captureStdout(t, func() int { return run([]string{"ask", "-m", "custom", "--no-stream", "hi"}) })
	if gotModel != "custom" {
		t.Fatalf("model = %q, want custom (default_alias=%q)", gotModel, "default-alias")
	}
}

func TestRunUnknownCommand(t *testing.T) {
	if code := run([]string{"bogus"}); code == 0 {
		t.Fatal("unknown command must fail")
	}
}
