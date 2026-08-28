package main

import (
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

func TestRunUnknownCommand(t *testing.T) {
	if code := run([]string{"bogus"}); code == 0 {
		t.Fatal("unknown command must fail")
	}
}
