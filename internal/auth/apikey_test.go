package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

func TestKeyDigestHashesPlaintext(t *testing.T) {
	sum := sha256.Sum256([]byte("sk-abc"))
	if got := mustDigest(t, "sk-abc"); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %q", got)
	}
}

func TestKeyDigestAcceptsConfiguredHash(t *testing.T) {
	sum := sha256.Sum256([]byte("sk-abc"))
	want := hex.EncodeToString(sum[:])
	got, err := keyDigest("sha256:" + want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
	// Upper-case hex is normalized.
	got, err = keyDigest("sha256:" + strings.ToUpper(want))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
}

func TestKeyDigestRejectsMalformedHash(t *testing.T) {
	for _, raw := range []string{"sha256:abc", "sha256:" + strings.Repeat("z", 64), "sha256:"} {
		if _, err := keyDigest(raw); err == nil {
			t.Fatalf("keyDigest(%q) should fail", raw)
		}
	}
}

func TestAPIKeyAuthenticatorAcceptsPlaintextConfiguredKey(t *testing.T) {
	authenticator, err := New(config.AuthConfig{Mode: "api-key"}, []config.TeamConfig{{
		ID:      "team-a",
		APIKeys: []config.APIKeyConfig{{ID: "key-a", Key: "sk-test-key-1234567890"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := testRequest("Bearer sk-test-key-1234567890")
	principal, err := authenticator.Authenticate(req.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if principal.APIKeyID != "key-a" {
		t.Fatalf("principal = %+v", principal)
	}
}

func TestAPIKeyAuthenticatorAcceptsHashedConfiguredKey(t *testing.T) {
	sum := sha256.Sum256([]byte("sk-hashed-key-1234567890"))
	authenticator, err := New(config.AuthConfig{Mode: "api-key"}, []config.TeamConfig{{
		ID: "team-a",
		APIKeys: []config.APIKeyConfig{{
			ID:  "key-a",
			Key: "sha256:" + hex.EncodeToString(sum[:]),
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := testRequest("Bearer sk-hashed-key-1234567890")
	principal, err := authenticator.Authenticate(req.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if principal.APIKeyID != "key-a" {
		t.Fatalf("principal = %+v", principal)
	}
	// The wrong plaintext must not authenticate against the digest.
	if _, err := authenticator.Authenticate(testRequest("Bearer sk-hashed-key-1234567891").Context(), testRequest("Bearer sk-hashed-key-1234567891")); err != ErrUnauthorized {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

func TestAPIKeyAuthenticatorRejectsInvalidDigestConfig(t *testing.T) {
	if _, err := New(config.AuthConfig{Mode: "api-key"}, []config.TeamConfig{{
		ID:      "team-a",
		APIKeys: []config.APIKeyConfig{{ID: "key-a", Key: "sha256:nothex"}},
	}}); err == nil {
		t.Fatal("expected error for malformed sha256 config")
	}
}

func mustDigest(t *testing.T, raw string) string {
	t.Helper()
	d, err := keyDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
