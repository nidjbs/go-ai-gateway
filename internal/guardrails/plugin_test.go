package guardrails

import (
	"net/http"
	"testing"

	"github.com/nidjbs/go-ai-gateway/internal/plugin"
)

// injectionText (defined in middleware_test.go) carries three rule matches and
// reaches the 0.75 block threshold; reused here so plugin and middleware tests
// exercise the same payload.

func TestPluginDisabledPassesThrough(t *testing.T) {
	p := NewPlugin(Config{Enabled: false, Mode: "block", Threshold: 0.75}, nil, nil)
	ctx := &plugin.Context{Endpoint: "chat.completions", Body: []byte(`{"messages":[{"role":"user","content":"` + injectionText + `"}]}`)}
	if err := p.BeforeRequest(ctx); err != nil {
		t.Fatalf("disabled plugin must pass through, got %v", err)
	}
}

func TestPluginRejectsBlockedPrompt(t *testing.T) {
	p := NewPlugin(Config{Enabled: true, Mode: "block", Threshold: 0.75, Tracker: DefaultTrackerConfig()}, nil, nil)
	body := []byte(`{"messages":[{"role":"user","content":"` + injectionText + `"}]}`)
	ctx := &plugin.Context{Endpoint: "chat.completions", Body: body}
	err := p.BeforeRequest(ctx)
	re, ok := plugin.AsRejection(err)
	if !ok {
		t.Fatalf("err = %v, want RejectionError", err)
	}
	if re.Status() != http.StatusTooManyRequests || re.Code != "prompt_injection_detected" {
		t.Errorf("rejection = %+v, want 429/prompt_injection_detected", re)
	}
}

func TestPluginFlagModeDoesNotReject(t *testing.T) {
	p := NewPlugin(Config{Enabled: true, Mode: "flag", Threshold: 0.75, Tracker: DefaultTrackerConfig()}, nil, nil)
	ctx := &plugin.Context{Endpoint: "chat.completions", Body: []byte(`{"messages":[{"role":"user","content":"` + injectionText + `"}]}`)}
	if err := p.BeforeRequest(ctx); err != nil {
		t.Fatalf("flag mode must not reject, got %v", err)
	}
	if _, ok := ctx.Value("guardrails_scan_result").(ScanResult); !ok {
		t.Error("flag mode must stash the scan result for later plugins")
	}
}

func TestPluginSkipsEmbeddings(t *testing.T) {
	p := NewPlugin(Config{Enabled: true, Mode: "block", Threshold: 0.75}, nil, nil)
	ctx := &plugin.Context{Endpoint: "embeddings", Body: []byte(`{"input":"whatever"}`)}
	if err := p.BeforeRequest(ctx); err != nil {
		t.Fatalf("embeddings must be skipped, got %v", err)
	}
}

func TestPluginAllowlistBypassesScan(t *testing.T) {
	p := NewPlugin(Config{Enabled: true, Mode: "block", Threshold: 0.75, Allowlist: []string{"red-team-benchmark"}}, nil, nil)
	body := []byte(`{"messages":[{"role":"user","content":"red-team-benchmark ` + injectionText + `"}]}`)
	ctx := &plugin.Context{Endpoint: "chat.completions", Body: body}
	if err := p.BeforeRequest(ctx); err != nil {
		t.Fatalf("allowlisted payload must pass, got %v", err)
	}
}

func TestPluginRejectsWithRetryAfterOnTrackerBlock(t *testing.T) {
	p := NewPlugin(Config{Enabled: true, Mode: "block", Threshold: 0.75, Tracker: DefaultTrackerConfig()}, nil, nil)
	// Repeated block-worthy requests trip the tracker's penalty window. The
	// tracker only counts authenticated keys, so the context carries one.
	base := &plugin.Context{Endpoint: "chat.completions", APIKeyID: "key-1", Body: []byte(`{"messages":[{"role":"user","content":"` + injectionText + `"}]}`)}
	for range 3 {
		_ = p.BeforeRequest(base)
	}
	ctx := &plugin.Context{Endpoint: "chat.completions", APIKeyID: "key-1", Body: []byte(`{"messages":[{"role":"user","content":"` + injectionText + `"}]}`)}
	err := p.BeforeRequest(ctx)
	re, ok := plugin.AsRejection(err)
	if !ok {
		t.Fatalf("err = %v, want RejectionError", err)
	}
	if re.Code != "injection_tracker_blocked" {
		t.Fatalf("code = %q, want injection_tracker_blocked", re.Code)
	}
	if re.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want positive", re.RetryAfter)
	}
}

func TestPluginStoresScanResultForResponses(t *testing.T) {
	p := NewPlugin(Config{Enabled: true, Mode: "flag", Threshold: 0.75}, nil, nil)
	ctx := &plugin.Context{Endpoint: "responses", Body: []byte(`{"input":"` + injectionText + `"}`)}
	if err := p.BeforeRequest(ctx); err != nil {
		t.Fatalf("flag mode must not reject, got %v", err)
	}
	result, ok := ctx.Value("guardrails_scan_result").(ScanResult)
	if !ok {
		t.Fatal("responses scan result missing")
	}
	if result.Action != "block" {
		t.Fatalf("responses scan action = %q, want block", result.Action)
	}
}
