package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"example.com/light-llm-gateway/internal/apierr"
	"example.com/light-llm-gateway/internal/auth"
	"example.com/light-llm-gateway/internal/circuitbreaker"
	"example.com/light-llm-gateway/internal/concurrency"
	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/guardrails"
	"example.com/light-llm-gateway/internal/metrics"
	"example.com/light-llm-gateway/internal/provider"
	"example.com/light-llm-gateway/internal/ratelimit"
	"example.com/light-llm-gateway/internal/usage"
)

// maxPerKeyLimiters caps the per-key concurrency limiter map. Without a bound
// a long-running gateway would accumulate one entry per distinct API key it
// ever sees. 10k entries is comfortably above any realistic tenant count and
// keeps the per-key channel buffers (~maxConcurrentPerKey slots) from
// ballooning memory under credential-spray attacks.
const maxPerKeyLimiters = 10_000

type Deps struct {
	Config        *config.Config
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
}

type readiness struct {
	startedAt time.Time
	waitTime  time.Duration
	draining  atomic.Bool
}

func (r *readiness) healthz(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func (r *readiness) readyz(w http.ResponseWriter, _ *http.Request) {
	if r.draining.Load() || (r.waitTime > 0 && time.Since(r.startedAt) < r.waitTime) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type Server struct {
	HTTP                *http.Server
	Ops                 http.Handler
	ready               *readiness
	config              *config.Config
	logger              *slog.Logger
	limiter             ratelimit.Limiter
	quotaStore          ratelimit.QuotaStore
	concurrency         *concurrency.Limiter
	keyLock             sync.Mutex
	keyLimits           map[string]*concurrency.Limiter
	now                 func() time.Time
	apiKeyLimit         map[string]config.KeyLimits
	closer              io.Closer // optional sink/handle released on graceful shutdown
	guardrailMiddleware func(next http.Handler) http.Handler
}

// keyLimiter returns the per-key concurrency limiter for keyID, allocating
// one on first use. Returns a nil limiter when the configured per-key cap is
// zero (= unlimited).
//
// The map is bounded: when it exceeds maxPerKeyLimiters, an arbitrary old
// entry is evicted to make room. Eviction is intentionally crude — picking a
// least-recently-used entry would add a second structure — because in the
// steady state the working set is a small fraction of the cap, so the cost
// of an occasional collision is bounded.
func (s *Server) keyLimiter(keyID string) *concurrency.Limiter {
	if s.config.Server.MaxConcurrentPerKey <= 0 {
		return nil
	}
	s.keyLock.Lock()
	defer s.keyLock.Unlock()
	l, ok := s.keyLimits[keyID]
	if !ok {
		l = concurrency.New(s.config.Server.MaxConcurrentPerKey)
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
	if deps.Authenticator == nil {
		deps.Authenticator = auth.NoopAuthenticator{}
	}
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
	if deps.Provider == nil {
		deps.Provider = provider.NewClient()
	}
	if deps.Config.Tracing.Enabled {
		// Wrap the per-provider transports so the OTel SDK can emit client
		// spans for each upstream call independently — pools are isolated by
		// provider type so the tracing transport is applied per-provider, not
		// globally.
		for _, ptype := range deps.Provider.RegisteredTypes() {
			existing := deps.Provider.HTTPClient(ptype)
			deps.Provider.SetHTTPClient(ptype, &http.Client{
				Transport: otelhttp.NewTransport(existing.Transport),
				Timeout:   existing.Timeout,
			})
		}
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
	if deps.APIKeyLimits == nil {
		deps.APIKeyLimits = map[string]config.KeyLimits{}
	}
	if deps.Breaker == nil {
		if deps.Config.CircuitBreaker.Enabled {
			deps.Breaker = circuitbreaker.New(circuitbreaker.Config{
				FailureThreshold:         deps.Config.CircuitBreaker.FailureThreshold,
				OpenDuration:             deps.Config.CircuitBreaker.OpenDuration,
				HalfOpenMaxRequests:      deps.Config.CircuitBreaker.HalfOpenMaxRequests,
				HalfOpenSuccessThreshold: deps.Config.CircuitBreaker.HalfOpenSuccessThreshold,
			})
		} else {
			deps.Breaker = circuitbreaker.Noop{}
		}
	}

	ready := &readiness{startedAt: time.Now(), waitTime: deps.Config.ReadyzWaitTime}

	// Initialize guardrails middleware (off by default; explicit opt-in).
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
	guardrailMW := guardrails.NewMiddleware(guardrails.Config{
		Enabled:   guardrailCfg.Enabled,
		Mode:      guardrailCfg.Mode,
		Threshold: guardrailCfg.Threshold,
		Tracker:   trackerPolicy,
	}, tracker, deps.Logger)

	server := &Server{
		config:              deps.Config,
		logger:              deps.Logger,
		concurrency:         concurrency.New(deps.Config.Server.MaxConcurrentRequests),
		keyLimits:           make(map[string]*concurrency.Limiter),
		now:                 deps.Now,
		apiKeyLimit:         deps.APIKeyLimits,
		closer:              sinkCloser(usageSink),
		guardrailMiddleware: guardrailMW.Handle,
	}
	handlers := handler{config: deps.Config, logger: deps.Logger, authenticator: deps.Authenticator, usageSink: usageSink, provider: deps.Provider, metrics: deps.Metrics, limiter: limiter, quotaStore: quotaStore, breaker: deps.Breaker, keyConcurrency: server.keyLimiter, now: deps.Now, apiKeyLimits: deps.APIKeyLimits}
	ops := chi.NewRouter()
	ops.Get("/healthz", ready.healthz)
	ops.Get("/livez", ready.healthz)
	ops.Get("/readyz", ready.readyz)
	ops.Handle("/metrics", deps.Metrics.Handler())

	router := chi.NewRouter()
	router.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, clientInfoMiddleware)
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
		r.Use(server.guardrailMiddleware)
		r.Get("/v1/models", handlers.models)
		r.Post("/v1/chat/completions", handlers.chat)
		r.Post("/v1/embeddings", handlers.embeddings)
	})

	server.HTTP = &http.Server{Addr: deps.Config.Listen, Handler: router, ReadHeaderTimeout: 10 * time.Second}
	server.Ops = ops
	server.ready = ready
	server.limiter = limiter
	server.quotaStore = quotaStore
	return server, nil
}

// concurrencyMiddleware rejects requests when the gateway-level in-flight cap
// is reached. Health probes are exempt so a saturated gateway still drains
// cleanly behind a load balancer.
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

// sinkCloser returns an io.Closer iff sink implements it (e.g. SQLSink closes
// its database handle). NoopSink and AuditSink fall through and return nil;
// their lifecycle is process-bound and needs no explicit close.
func sinkCloser(sink usage.Sink) io.Closer {
	if c, ok := sink.(io.Closer); ok {
		return c
	}
	return nil
}

// pickLimiter resolves the Limiter in priority order: explicit Deps injection,
// config-driven driver lookup, then the in-process memory fallback.
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

// pickUsageSink resolves the usage sink. The production behavior is to write
// audit records to slog; binaries that want a different sink can inject it
// directly via Deps or register a custom driver in config.
func pickUsageSink(deps Deps) (usage.Sink, error) {
	if deps.UsageSink != nil {
		return deps.UsageSink, nil
	}
	if driver := deps.Config.Usage.Driver; driver != "" {
		return usage.Registry.Build(driver, deps.Config.Usage.Options)
	}
	return usage.NewAuditSink(deps.Logger), nil
}

// pickGuardrailTracker builds the per-key injection-frequency tracker. The
// empty Driver falls back to the in-process map (single-replica only);
// setting Driver="redis" shares state across replicas via the
// guardrails.TrackerRegistry.
func pickGuardrailTracker(driver config.StorageDriver, policy guardrails.TrackerConfig) (guardrails.Tracker, error) {
	name := driver.Driver
	if name == "" {
		name = "memory"
	}
	// Stash the policy inside the options map under a private key so every
	// factory can pull it back out without taking a second argument.
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

func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	authenticator, err := auth.New(cfg.Auth, cfg.Teams)
	if err != nil {
		return err
	}
	limits := make(map[string]config.KeyLimits, len(cfg.Teams))
	for _, team := range cfg.Teams {
		for _, key := range team.APIKeys {
			limits[key.ID] = key.Limits
		}
	}
	server, err := New(Deps{Config: cfg, Logger: logger, Authenticator: authenticator, APIKeyLimits: limits})
	if err != nil {
		return err
	}
	health := &http.Server{Addr: cfg.Healthz, Handler: server.Ops, ReadHeaderTimeout: 10 * time.Second}
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
