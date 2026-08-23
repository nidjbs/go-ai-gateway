package dlp

import (
	"strings"
	"testing"
)

func mustDetector(t *testing.T, cfg Config) *Detector {
	t.Helper()
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestScanDetectsCommonPII(t *testing.T) {
	d := mustDetector(t, Config{Enabled: true, Mode: "mask"})
	cases := []struct{ name, text, wantPattern string }{
		{"email", "contact alice@example.com now", "email"},
		{"phone", "call me at (415) 555-0134 today", "phone"},
		{"card", "card 4111 1111 1111 1111 expires", "credit_card"},
		{"ssn", "ssn 123-45-6789 on file", "ssn"},
		{"secret", "token sk-proj-abcdefghijklmnopqrstuvwxyz leaked", "secret_key"},
		{"ip", "server 192.168.1.100 is up", "ip_address"},
		{"clean", "the quick brown fox jumps", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := d.Scan(tc.text)
			if tc.wantPattern == "" {
				if len(hits) != 0 {
					t.Fatalf("expected no hits, got %+v", hits)
				}
				return
			}
			found := false
			for _, h := range hits {
				if h.Pattern == tc.wantPattern {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected pattern %q in %+v", tc.wantPattern, hits)
			}
		})
	}
}

func TestMaskReplacesMatches(t *testing.T) {
	d := mustDetector(t, Config{Enabled: true, Mode: "mask"})
	text := "email alice@example.com and phone (415) 555-0134"
	masked, hits := d.Mask(text)
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %+v", hits)
	}
	if strings.Contains(masked, "alice@example.com") || strings.Contains(masked, "415") {
		t.Fatalf("masked output still contains PII: %q", masked)
	}
	if !strings.Contains(masked, "[REDACTED]") {
		t.Fatalf("expected mask text in output: %q", masked)
	}
	// Prefix/suffix preserved.
	if !strings.HasPrefix(masked, "email ") || !strings.HasSuffix(masked, "") {
		t.Fatalf("context lost: %q", masked)
	}
}

func TestMaskCustomMaskText(t *testing.T) {
	d := mustDetector(t, Config{Enabled: true, Mode: "mask", MaskText: "<pii>"})
	masked, _ := d.Mask("a@b.com here")
	if masked != "<pii> here" {
		t.Fatalf("got %q", masked)
	}
}

func TestMaskOverlappingPatterns(t *testing.T) {
	// A phone number inside a longer string must be fully replaced even
	// when another pattern (credit_card) also overlaps it.
	d := mustDetector(t, Config{Enabled: true, Mode: "mask"})
	masked, _ := d.Mask("digits 123-45-6789 end")
	if strings.Contains(masked, "6789") {
		t.Fatalf("ssn fragment leaked: %q", masked)
	}
}

func TestStreamMaskerCrossChunkBoundary(t *testing.T) {
	d := mustDetector(t, Config{Enabled: true, Mode: "mask", CarrySize: 64})
	m := d.NewStreamMasker()
	// Split an email across chunk boundaries byte by byte.
	chunks := []string{"user-", "name@exa", "mple.com", " done"}
	var out strings.Builder
	for _, c := range chunks {
		out.Write(m.Process([]byte(c)))
	}
	out.Write(m.Flush())
	got := out.String()
	if strings.Contains(got, "@exa") || strings.Contains(got, "example.com") {
		t.Fatalf("cross-chunk email leaked: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("expected mask in stream output: %q", got)
	}
}

func TestStreamMaskerSingleChunk(t *testing.T) {
	d := mustDetector(t, Config{Enabled: true, Mode: "mask", CarrySize: 64})
	m := d.NewStreamMasker()
	out := string(m.Process([]byte("call 415-555-0134 now")))
	if strings.Contains(out, "555-0134") {
		t.Fatalf("phone leaked in chunk: %q", out)
	}
}

func TestRejectSignal(t *testing.T) {
	d := mustDetector(t, Config{Enabled: true, Mode: "reject"})
	m := d.NewStreamMasker()
	if hits := m.Reject([]byte("email a@b.com")); len(hits) == 0 {
		t.Fatal("expected hit in reject mode")
	}
	// A fresh masker over clean content must not trip reject.
	m2 := d.NewStreamMasker()
	if hits := m2.Reject([]byte("all clear")); len(hits) != 0 {
		t.Fatalf("unexpected reject: %+v", hits)
	}
	// Cross-chunk reject: split the email across two chunks.
	m3 := d.NewStreamMasker()
	if hits := m3.Reject([]byte("user@exa")); len(hits) != 0 {
		t.Fatalf("partial email must not reject: %+v", hits)
	}
	if hits := m3.Reject([]byte("mple.com")); len(hits) == 0 {
		t.Fatal("cross-chunk email must reject")
	}
}

func TestUnknownPatternFails(t *testing.T) {
	if _, err := New(Config{Enabled: true, Mode: "mask", Patterns: []string{"bogus"}}); err == nil {
		t.Fatal("expected error for unknown pattern")
	}
}

func TestDisabledDetectorIsNoop(t *testing.T) {
	d := mustDetector(t, Config{Enabled: false, Mode: "mask"})
	if hits := d.Scan("alice@example.com"); len(hits) != 0 {
		t.Fatalf("disabled detector must not scan: %+v", hits)
	}
	if m, _ := d.Mask("alice@example.com"); m != "alice@example.com" {
		t.Fatalf("disabled mask must passthrough: %q", m)
	}
}
