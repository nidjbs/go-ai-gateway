package auth

import (
	"net/http"
	"testing"

	"example.com/light-llm-gateway/internal/config"
)

func TestStaticAuthenticatorAcceptsOnlyConfiguredBearerToken(t *testing.T) {
	t.Setenv("GATEWAY_TOKEN", "test-token")
	authenticator, err := New(config.AuthConfig{Mode: "static", TokenEnv: "GATEWAY_TOKEN"})
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

func testRequest(authorization string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://gateway.test/v1/models", nil)
	r.Header.Set("Authorization", authorization)
	return r
}
