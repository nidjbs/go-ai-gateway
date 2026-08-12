package gateway

import (
	"context"
	"net/http"
	"strings"
)

// ctxKey is an unexported type used as a context key inside the gateway so it
// cannot collide with keys defined by other packages.
type ctxKey struct{ name string }

var (
	clientIPKey  = ctxKey{"client_ip"}
	userAgentKey = ctxKey{"user_agent"}
)

// clientInfoMiddleware extracts the source IP (resolved through chi's
// middleware.RealIP, which already ran earlier in the chain) and the
// User-Agent header and stashes them on the request context. Handlers read
// them via clientIP / userAgent without re-parsing headers on every call.
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

// requestEndpoint maps the request path to the stable endpoint name used in
// audit events. Unknown paths fall through verbatim so a future endpoint still
// gets recorded, just under its URL path.
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
