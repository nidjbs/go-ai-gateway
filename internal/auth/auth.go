package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"

	"example.com/light-llm-gateway/internal/config"
)

var ErrUnauthorized = errors.New("unauthorized")

type Principal struct{ Subject string }

type Authenticator interface {
	Authenticate(context.Context, *http.Request) (Principal, error)
}

type NoopAuthenticator struct{}

func (NoopAuthenticator) Authenticate(context.Context, *http.Request) (Principal, error) {
	return Principal{Subject: "anonymous"}, nil
}

type staticAuthenticator struct{ token string }

func New(cfg config.AuthConfig) (Authenticator, error) {
	if cfg.Mode == "" || cfg.Mode == "none" {
		return NoopAuthenticator{}, nil
	}
	if cfg.Mode != "static" {
		return nil, errors.New("unsupported auth mode")
	}
	token := strings.TrimSpace(os.Getenv(cfg.TokenEnv))
	if token == "" {
		return nil, errors.New("static authentication token environment variable is unset")
	}
	return staticAuthenticator{token: token}, nil
}

func (a staticAuthenticator) Authenticate(_ context.Context, r *http.Request) (Principal, error) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(value, "Bearer ") {
		return Principal{}, ErrUnauthorized
	}
	provided := strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) != 1 {
		return Principal{}, ErrUnauthorized
	}
	return Principal{Subject: "static"}, nil
}
