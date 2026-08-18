package gateway

import (
	"context"
	"net/http"
	"strings"
)

// ctxKey is an unexported type used as a context key to avoid collision.
type ctxKey struct{ name string }

var (
	clientIPKey  = ctxKey{"client_ip"}
	userAgentKey = ctxKey{"user_agent"}
)

// clientInfoMiddleware extracts the client IP (via chi RealIP) and User-Agent
// onto the request context for later handlers to read.
func clientInfoMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientIPKey, r.RemoteAddr)
		ctx = context.WithValue(ctx, userAgentKey, r.Header.Get("User-Agent"))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func clientIP(ctx context.Context) string {
	if v, ok := ctx.Value(clientIPKey).(string); ok {
		return v
	}
	return ""
}

func userAgent(ctx context.Context) string {
	if v, ok := ctx.Value(userAgentKey).(string); ok {
		return v
	}
	return ""
}

// requestEndpoint maps the request path to the stable endpoint name in audit events.
func requestEndpoint(r *http.Request) string {
	switch {
	case strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
		return "chat.completions"
	case strings.HasSuffix(r.URL.Path, "/v1/embeddings"):
		return "embeddings"
	case strings.HasSuffix(r.URL.Path, "/v1/models"):
		return "models"
	}
	return r.URL.Path
}
