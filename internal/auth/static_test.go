package auth

import (
	"net/http"
	"testing"

	"example.com/light-llm-gateway/internal/config"
)

func TestStaticAuthenticatorAcceptsOnlyConfiguredBearerToken(t *testing.T) {
	t.Setenv("GATEWAY_TOKEN", "test-token")
	authenticator, err := New(config.AuthConfig{Mode: "static", TokenEnv: "GATEWAY_TOKEN"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ok := testRequest("Bearer test-token")
	if _, err := authenticator.Authenticate(ok.Context(), ok); err != nil {
		t.Fatal(err)
	}
	denied := testRequest("Bearer wrong")
	if _, err := authenticator.Authenticate(denied.Context(), denied); err != ErrUnauthorized {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

func TestAPIKeyAuthenticatorAcceptsConfiguredBearerToken(t *testing.T) {
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
	if principal.APIKeyID != "key-a" || principal.TeamID != "team-a" {
		t.Fatalf("principal = %+v", principal)
	}
}

func TestAPIKeyAuthenticatorRejectsUnknownKey(t *testing.T) {
	authenticator, err := New(config.AuthConfig{Mode: "api-key"}, []config.TeamConfig{{
		ID:      "team-a",
		APIKeys: []config.APIKeyConfig{{ID: "key-a", Key: "sk-test-key-1234567890"}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	req := testRequest("Bearer sk-other")
	if _, err := authenticator.Authenticate(req.Context(), req); err != ErrUnauthorized {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

func TestAPIKeyAuthenticatorRejectsAbsentHeader(t *testing.T) {
	authenticator, err := New(config.AuthConfig{Mode: "api-key"}, []config.TeamConfig{{
		ID:      "team-a",
		APIKeys: []config.APIKeyConfig{{ID: "key-a", Key: "sk-test-key-1234567890"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://gateway.test/v1/models", nil)
	if _, err := authenticator.Authenticate(req.Context(), req); err != ErrUnauthorized {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

func testRequest(authorization string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://gateway.test/v1/models", nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r
}
