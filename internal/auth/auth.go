package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

var ErrUnauthorized = errors.New("unauthorized")

type Principal struct {
	Subject  string
	APIKeyID string
	TeamID   string
}

type Authenticator interface {
	Authenticate(context.Context, *http.Request) (Principal, error)
}

type NoopAuthenticator struct{}

func (NoopAuthenticator) Authenticate(context.Context, *http.Request) (Principal, error) {
	return Principal{Subject: "anonymous"}, nil
}

type staticAuthenticator struct{ token string }

func New(cfg config.AuthConfig, teams []config.TeamConfig) (Authenticator, error) {
	switch cfg.Mode {
	case "", "none":
		return NoopAuthenticator{}, nil
	case "static":
		token := strings.TrimSpace(os.Getenv(cfg.TokenEnv))
		if token == "" {
			return nil, errors.New("static authentication token environment variable is unset")
		}
		return staticAuthenticator{token: token}, nil
	case "api-key":
		return newAPIKeyAuthenticator(teams)
	default:
		return nil, errors.New("unsupported auth mode")
	}
}

func (a staticAuthenticator) Authenticate(_ context.Context, r *http.Request) (Principal, error) {
	provided, ok := bearerToken(r)
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) != 1 {
		return Principal{}, ErrUnauthorized
	}
	return Principal{Subject: "static"}, nil
}

func bearerToken(r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(value, "Bearer ") {
		return "", false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	if provided == "" {
		return "", false
	}
	return provided, true
}

type principalContextKey struct{}

func ContextWithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
