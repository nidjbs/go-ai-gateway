package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/nidjbs/go-ai-gateway/internal/apierr"
	"github.com/nidjbs/go-ai-gateway/internal/auth"
	"github.com/nidjbs/go-ai-gateway/internal/circuitbreaker"
	"github.com/nidjbs/go-ai-gateway/internal/concurrency"
	"github.com/nidjbs/go-ai-gateway/internal/config"
	"github.com/nidjbs/go-ai-gateway/internal/events"
	"github.com/nidjbs/go-ai-gateway/internal/guardrails"
	"github.com/nidjbs/go-ai-gateway/internal/metrics"
	"github.com/nidjbs/go-ai-gateway/internal/plugin"
	"github.com/nidjbs/go-ai-gateway/internal/provider"
	"github.com/nidjbs/go-ai-gateway/internal/ratelimit"
	"github.com/nidjbs/go-ai-gateway/internal/routing"
	"github.com/nidjbs/go-ai-gateway/internal/usage"
	"github.com/nidjbs/go-ai-gateway/internal/version"
)

// maxPerKeyLimiters caps the per-key limiter map to prevent unbounded growth
// under credential-spray attacks.
const maxPerKeyLimiters = 10_000

type Deps struct {
	Config        *config.Config
	ConfigPath    string // config file path, used by POST /admin/reload
	Logger        *slog.Logger
	Authenticator auth.Authenticator
	UsageSink     usage.Sink
	Provider      *provider.Client
	Metrics       *metrics.Recorder
	Limiter       ratelimit.Limiter
	QuotaStore    ratelimit.QuotaStore
	Breaker       circuitbreaker.Breaker
	Now           func() time.Time
	APIKeyLimits  map[string]config.KeyLimits
	Events        *events.Hub // optional; built from Config.Events when nil
}

type readiness struct {
	startedAt time.Time
	waitTime  time.Duration
	draining  atomic.Bool
	checks    []func() error
}

func (r *readiness) healthz(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func (r *readiness) readyz(w http.ResponseWriter, _ *http.Request) {
	if r.draining.Load() || (r.waitTime > 0 && time.Since(r.startedAt) < r.waitTime) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	for _, check := range r.checks {
		if err := check(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

type Server struct {
	HTTP        *http.Server
	Ops         http.Handler
	ready       *readiness
	rt          *atomic.Pointer[runtime] // hot-reloadable config snapshot
	logger      *slog.Logger
	limiter     ratelimit.Limiter
	quotaStore  ratelimit.QuotaStore
	concurrency *concurrency.Limiter
	keyLock     sync.Mutex
	keyLimits   map[string]*concurrency.Limiter
	now         func() time.Time
	closer      io.Closer // optional sink/handle released on graceful shutdown
}

// keyLimiter returns the per-key concurrency limiter for keyID, allocating
// one on first use. Returns nil when the configured per-key cap is zero.
// The map is bounded; excess entries are evicted crudely (arbitrary).
func (s *Server) keyLimiter(keyID string) *concurrency.Limiter {
	if s.rt.Load().config.Server.MaxConcurrentPerKey <= 0 {
		return nil
	}
	s.keyLock.Lock()
	defer s.keyLock.Unlock()
	l, ok := s.keyLimits[keyID]
	if !ok {
		l = concurrency.New(s.rt.Load().config.Server.MaxConcurrentPerKey)
		s.keyLimits[keyID] = l
		if len(s.keyLimits) > maxPerKeyLimiters {
			for victim := range s.keyLimits {
				if victim == keyID {
					continue
				}
				delete(s.keyLimits, victim)
				break
			}
		}
	}
	return l
}

func New(deps Deps) (*Server, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	// Leave deps.Authenticator nil so buildRuntime derives it from cfg.Auth
	// (auth.mode static/api-key); defaulting to Noop here would shadow config.
	limiter, err := pickLimiter(deps)
	if err != nil {
		return nil, err
	}
	quotaStore, err := pickQuotaStore(deps)
	if err != nil {
		return nil, err
	}
	usageSink, err := pickUsageSink(deps)
	if err != nil {
		return nil, err
	}
	if deps.Metrics == nil {
		var mErr error
		deps.Metrics, mErr = metrics.New()
		if mErr != nil {
			return nil, mErr
		}
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	eventsHub, err := pickEventsHub(deps)
	if err != nil {
		return nil, err
	}
	ready := &readiness{startedAt: deps.Now(), waitTime: deps.Config.ReadyzWaitTime}
	for _, driver := range []config.StorageDriver{deps.Config.RateLimit, deps.Config.Quota} {
		if driver.Driver != "redis" {
			continue
		}
		ping, err := ratelimit.NewRedisPinger(driver.Options)
		if err != nil {
			return nil, fmt.Errorf("readiness probe (%s): %w", driver.Driver, err)
		}
		ready.checks = append(ready.checks, ping)
	}

	guardrailCfg := deps.Config.Guardrails
	if guardrailCfg.Mode == "" {
		guardrailCfg.Mode = "off"
	}
	trackerPolicy := guardrails.TrackerConfig{
		MaxAttempts: guardrailCfg.Tracker.MaxAttempts,
		Window:      time.Duration(guardrailCfg.Tracker.WindowSec) * time.Second,
		Penalty:     time.Duration(guardrailCfg.Tracker.PenaltySec) * time.Second,
	}
	tracker, err := pickGuardrailTracker(guardrailCfg.Tracker.Driver, trackerPolicy)
	if err != nil {
		return nil, fmt.Errorf("guardrails tracker: %w", err)
	}
	// Guardrails run as a before_request plugin inside the request handlers
	// (after parsing, before the provider call), replacing the previous HTTP
	// middleware. The middleware form remains available for tests.
	guardrailsPlugin := guardrails.NewPlugin(guardrails.Config{
		Enabled:      guardrailCfg.Enabled,
		Mode:         guardrailCfg.Mode,
		Threshold:    guardrailCfg.Threshold,
		Tracker:      trackerPolicy,
		Allowlist:    guardrailCfg.Allowlist,
		MaxBodyBytes: deps.Config.Server.BodyLimit(),
	}, tracker, deps.Logger)
	pluginManager := plugin.NewManager(deps.Logger)
	pluginManager.Add(guardrailsPlugin)

	rt := &atomic.Pointer[runtime]{}
	current, err := buildRuntime(deps.ConfigPath, deps.Config, deps)
	if err != nil {
		return nil, err
	}
	rt.Store(current)
	closer := sinkCloser(usageSink)
	if eventsHub != nil {
		hub := eventsHub
		sink := closer
		closer = closerFunc(func() error {
			hub.Close()
			if sink != nil {
				return sink.Close()
			}
			return nil
		})
	}
	server := &Server{
		rt:          rt,
		logger:      deps.Logger,
		concurrency: concurrency.New(deps.Config.Server.MaxConcurrentRequests),
		keyLimits:   make(map[string]*concurrency.Limiter),
		now:         deps.Now,
		closer:      closer,
	}
	handlers := handler{rtPtr: rt, logger: deps.Logger, usageSink: usageSink, metrics: deps.Metrics, limiter: limiter, quotaStore: quotaStore, keyConcurrency: server.keyLimiter, now: deps.Now, idem: make(map[string]idemEntry), idemMu: &sync.Mutex{}, latency: routing.NewLatencyTracker(), plugins: pluginManager, events: eventsHub}
	ops := chi.NewRouter()
	if deps.Config.OpsTokenEnv != "" {
		token := strings.TrimSpace(os.Getenv(deps.Config.OpsTokenEnv))
		if token == "" {
			return nil, fmt.Errorf("ops_token_env %q is unset or empty", deps.Config.OpsTokenEnv)
		}
		ops.Use(opsTokenMiddleware(token))
	}
	ops.Get("/healthz", ready.healthz)
	ops.Get("/livez", ready.healthz)
	ops.Get("/readyz", ready.readyz)
	ops.Get("/version", versionHandler)
	ops.Handle("/metrics", deps.Metrics.Handler())
	if deps.Config.Admin.Enabled {
		token := strings.TrimSpace(os.Getenv(deps.Config.Admin.TokenEnv))
		ops.Group(func(r chi.Router) {
			r.Use(adminTokenMiddleware(token))
			r.Post("/admin/keys/revoke", handlers.revokeKey)
			r.Get("/admin/keys/revoked", handlers.listRevoked)
			r.Get("/admin/usage/summary", handlers.usageSummary)
			r.Get("/admin/usage/series", handlers.usageSeries)
			r.Post("/admin/reload", handlers.reloadConfig)
		})
	}

	router := chi.NewRouter()
	router.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, clientInfoMiddleware)
	if deps.Config.Server.AccessLog.Enabled {
		router.Use(accessLogMiddleware(deps.Logger, deps.Config.Server.AccessLog))
	}
	if deps.Config.Tracing.Enabled {
		router.Use(otelhttp.NewMiddleware("ai-gateway"))
	}
	router.Get("/healthz", ready.healthz)
	router.Get("/livez", ready.healthz)
	if deps.Config.Healthz == deps.Config.Listen {
		router.Get("/readyz", ready.readyz)
		router.Handle("/metrics", deps.Metrics.Handler())
	}
	router.Group(func(r chi.Router) {
		r.Use(server.concurrencyMiddleware)
		r.Use(handlers.authenticate)
		r.Get("/v1/models", handlers.models)
		r.Post("/v1/chat/completions", handlers.chat)
		r.Post("/v1/responses", handlers.responses)
		r.Post("/v1/embeddings", handlers.embeddings)
	})

	server.HTTP = &http.Server{
		Addr:              deps.Config.Listen,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       deps.Config.Server.ReadTimeout,
		IdleTimeout:       deps.Config.Server.IdleTimeout,
	}
	server.Ops = ops
	server.ready = ready
	server.limiter = limiter
	server.quotaStore = quotaStore
	return server, nil
}

// CheckReadiness runs all dependency readiness probes. The gateway calls it
// once at startup so a misconfigured backend fails fast, and readyz re-runs
// it per probe so orchestrators stop routing traffic when a backend drops.
// versionHandler reports build metadata for operations tooling. Values are
// injected at build time via -ldflags -X.
func versionHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"version":    version.Version,
		"commit":     version.Commit,
		"build_date": version.BuildDate,
	})
}

// opsTokenMiddleware requires a Bearer token (constant-time compare) on
// operational endpoints when ops_token_env is configured.
func opsTokenMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				apierr.Write(w, http.StatusUnauthorized, "invalid_api_key", "invalid_request_error", "Invalid ops token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) CheckReadiness() error {
	for _, check := range s.ready.checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// concurrencyMiddleware rejects requests when the gateway-level in-flight cap
// is reached. Health probes are exempt so a saturated gateway drains cleanly.
func (s *Server) concurrencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.concurrency.TryAcquire() {
			w.Header().Set("Retry-After", "1")
			apierr.Write(w, http.StatusServiceUnavailable, "server_overloaded", "server_error", "Server is overloaded, please retry")
			return
		}
		defer s.concurrency.Release()
		next.ServeHTTP(w, r)
	})
}

// sinkCloser returns an io.Closer iff sink implements it (e.g. SQLSink).
func sinkCloser(sink usage.Sink) io.Closer {
	if c, ok := sink.(io.Closer); ok {
		return c
	}
	return nil
}

// closerFunc adapts a function to io.Closer.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// pickEventsHub resolves the events hub: Deps injection, else built from
// Config.Events. Returns nil when no events are configured.
func pickEventsHub(deps Deps) (*events.Hub, error) {
	if deps.Events != nil {
		return deps.Events, nil
	}
	if len(deps.Config.Events.Webhooks) == 0 {
		return nil, nil
	}
	cfgs := make([]events.WebhookConfig, 0, len(deps.Config.Events.Webhooks))
	for _, w := range deps.Config.Events.Webhooks {
		types := make([]events.EventType, 0, len(w.Events))
		for _, t := range w.Events {
			types = append(types, events.EventType(t))
		}
		cfgs = append(cfgs, events.WebhookConfig{
			Name: w.Name, URL: w.URL, Headers: w.Headers, Events: types,
			Queue: w.Queue, Timeout: w.Timeout, Retries: w.Retries,
		})
	}
	return events.NewHub(cfgs, deps.Logger, deps.Metrics)
}

// pickLimiter resolves the Limiter: Deps injection, then config driver, then memory.
func pickLimiter(deps Deps) (ratelimit.Limiter, error) {
	if deps.Limiter != nil {
		return deps.Limiter, nil
	}
	if driver := deps.Config.RateLimit.Driver; driver != "" {
		return ratelimit.LimiterRegistry.Build(driver, deps.Config.RateLimit.Options)
	}
	return ratelimit.NewMemoryLimiter(), nil
}

// pickQuotaStore mirrors pickLimiter for the daily-quota tracker.
func pickQuotaStore(deps Deps) (ratelimit.QuotaStore, error) {
	if deps.QuotaStore != nil {
		return deps.QuotaStore, nil
	}
	if driver := deps.Config.Quota.Driver; driver != "" {
		return ratelimit.QuotaRegistry.Build(driver, deps.Config.Quota.Options)
	}
	return ratelimit.NewMemoryQuotaStore(), nil
}

// pickUsageSink resolves the usage sink; defaults to slog audit sink.
func pickUsageSink(deps Deps) (usage.Sink, error) {
	if deps.UsageSink != nil {
		return deps.UsageSink, nil
	}
	if driver := deps.Config.Usage.Driver; driver != "" {
		return usage.Registry.Build(driver, deps.Config.Usage.Options)
	}
	return usage.NewAuditSink(deps.Logger), nil
}

// pickGuardrailTracker builds the per-key injection-frequency tracker.
// Empty Driver falls back to in-process map; "redis" shares across replicas.
func pickGuardrailTracker(driver config.StorageDriver, policy guardrails.TrackerConfig) (guardrails.Tracker, error) {
	name := driver.Driver
	if name == "" {
		name = "memory"
	}
	merged := map[string]any{}
	for k, v := range driver.Options {
		merged[k] = v
	}
	merged["_policy"] = policy
	tracker, err := guardrails.TrackerRegistry.Build(name, merged)
	if err != nil {
		return nil, fmt.Errorf("guardrails tracker: %w", err)
	}
	return tracker, nil
}

func Run(ctx context.Context, cfg *config.Config, configPath string, logger *slog.Logger) error {
	server, err := New(Deps{Config: cfg, ConfigPath: configPath, Logger: logger})
	if err != nil {
		return err
	}
	// Fail fast when a configured dependency (e.g. Redis) is unreachable at
	// boot instead of silently degrading to fail-open at request time.
	if err := server.CheckReadiness(); err != nil {
		return fmt.Errorf("startup readiness: %w", err)
	}
	health := &http.Server{
		Addr:              cfg.Healthz,
		Handler:           server.Ops,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
	}
	errCh := make(chan error, 2)
	serve := func(s *http.Server) {
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}
	go serve(server.HTTP)
	if cfg.Healthz != cfg.Listen {
		go serve(health)
	}
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		server.ready.draining.Store(true)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var shutdownErrs []error
		if err := server.HTTP.Shutdown(shutdownCtx); err != nil {
			shutdownErrs = append(shutdownErrs, fmt.Errorf("api server shutdown: %w", err))
		}
		if err := health.Shutdown(shutdownCtx); err != nil {
			shutdownErrs = append(shutdownErrs, fmt.Errorf("health server shutdown: %w", err))
		}
		if server.closer != nil {
			if err := server.closer.Close(); err != nil {
				shutdownErrs = append(shutdownErrs, fmt.Errorf("usage sink close: %w", err))
			}
		}
		return errors.Join(shutdownErrs...)
	}
}
