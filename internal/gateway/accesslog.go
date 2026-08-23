package gateway

import (
	"context"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/auth"
	"github.com/nidjbs/go-ai-gateway/internal/config"
)

// accessLogKey carries a mutable state shared between the middleware and
// the inner handlers, which enrich it with principal/alias as they run.
type accessLogKey struct{}

type accessLogState struct {
	mu     sync.Mutex
	keyID  string
	teamID string
	alias  string
}

func (s *accessLogState) setPrincipal(p auth.Principal) {
	if s == nil || p.APIKeyID == "" {
		return
	}
	s.mu.Lock()
	s.keyID = p.APIKeyID
	s.teamID = p.TeamID
	s.mu.Unlock()
}

func (s *accessLogState) setAlias(alias string) {
	if s == nil || alias == "" {
		return
	}
	s.mu.Lock()
	s.alias = alias
	s.mu.Unlock()
}

func (s *accessLogState) snapshot() (keyID, teamID, alias string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyID, s.teamID, s.alias
}

// defaultAccessLogExcludes keeps probe noise out of the access log.
var defaultAccessLogExcludes = []string{"/healthz", "/livez", "/readyz", "/metrics", "/version", "/favicon.ico"}

// accessLogMiddleware logs one structured line per request after it
// completes: metadata only, never bodies. Sampling is deterministic on the
// request ID.
func accessLogMiddleware(logger *slog.Logger, cfg config.AccessLogConfig) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	ratio := cfg.SampleRatio
	if ratio <= 0 {
		ratio = 1.0
	}
	excludes := cfg.Exclude
	if len(excludes) == 0 {
		excludes = defaultAccessLogExcludes
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if excludedPath(excludes, r.URL.Path) || !sampledRequest(ratio, r) {
				next.ServeHTTP(w, r)
				return
			}
			state := &accessLogState{}
			ctx := context.WithValue(r.Context(), accessLogKey{}, state)
			sw := &statusWriter{ResponseWriter: w}
			started := time.Now()
			next.ServeHTTP(sw, r.WithContext(ctx))
			keyID, teamID, alias := state.snapshot()
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []slog.Attr{
				slog.String("request_id", requestID(r)),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int64("duration_ms", time.Since(started).Milliseconds()),
				slog.Int64("bytes_written", sw.bytes),
				slog.String("client_ip", clientIP(ctx)),
				slog.String("user_agent", userAgent(ctx)),
			}
			if keyID != "" {
				attrs = append(attrs, slog.String("api_key_id", keyID))
			}
			if teamID != "" {
				attrs = append(attrs, slog.String("team_id", teamID))
			}
			if alias != "" {
				attrs = append(attrs, slog.String("alias", alias))
			}
			logger.LogAttrs(context.Background(), slog.LevelInfo, "access", attrs...)
		})
	}
}

// statusWriter captures response status and byte count, passing through
// writes and flushes (streaming needs http.Flusher).
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func excludedPath(excludes []string, path string) bool {
	for _, e := range excludes {
		if e != "" && strings.HasPrefix(path, e) {
			return true
		}
	}
	return false
}

// sampledRequest deterministically samples on the request ID hash.
func sampledRequest(ratio float64, r *http.Request) bool {
	if ratio >= 1.0 {
		return true
	}
	if ratio <= 0.0 {
		return false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(requestID(r)))
	return float64(h.Sum32()%10000)/10000 < ratio
}

// accessLogStateFromContext returns the request's shared state, or nil.
func accessLogStateFromContext(ctx context.Context) *accessLogState {
	v, _ := ctx.Value(accessLogKey{}).(*accessLogState)
	return v
}

// recordAccessPrincipal enriches the access-log state with the principal.
func recordAccessPrincipal(ctx context.Context, p auth.Principal) {
	if s := accessLogStateFromContext(ctx); s != nil {
		s.setPrincipal(p)
	}
}

// recordAccessAlias enriches the access-log state with the resolved alias.
func recordAccessAlias(ctx context.Context, alias string) {
	if s := accessLogStateFromContext(ctx); s != nil {
		s.setAlias(alias)
	}
}
