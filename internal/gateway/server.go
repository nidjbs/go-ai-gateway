package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"example.com/light-llm-gateway/internal/auth"
	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/metrics"
	"example.com/light-llm-gateway/internal/provider"
	"example.com/light-llm-gateway/internal/ratelimit"
	"example.com/light-llm-gateway/internal/usage"
)

type Deps struct {
	Config        *config.Config
	Logger        *slog.Logger
	Authenticator auth.Authenticator
	UsageSink     usage.Sink
	Provider      *provider.Client
	Metrics       *metrics.Recorder
	Limiter       ratelimit.Limiter
	QuotaStore    ratelimit.QuotaStore
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
	HTTP        *http.Server
	Ops         http.Handler
	ready       *readiness
	config      *config.Config
	logger      *slog.Logger
	limiter     ratelimit.Limiter
	quotaStore  ratelimit.QuotaStore
	now         func() time.Time
	apiKeyLimit map[string]config.KeyLimits
}

func New(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Authenticator == nil {
		deps.Authenticator = auth.NoopAuthenticator{}
	}
	if deps.UsageSink == nil {
		deps.UsageSink = usage.NoopSink{}
	}
	if deps.Provider == nil {
		deps.Provider = provider.NewClient()
	}
	if deps.Metrics == nil {
		var err error
		deps.Metrics, err = metrics.New()
		if err != nil {
			panic(err)
		}
	}
	if deps.Limiter == nil {
		deps.Limiter = ratelimit.NewMemoryLimiter()
	}
	if deps.QuotaStore == nil {
		deps.QuotaStore = ratelimit.NewMemoryQuotaStore()
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.APIKeyLimits == nil {
		deps.APIKeyLimits = map[string]config.KeyLimits{}
	}

	ready := &readiness{startedAt: time.Now(), waitTime: deps.Config.ReadyzWaitTime}
	handlers := handler{config: deps.Config, logger: deps.Logger, authenticator: deps.Authenticator, usageSink: deps.UsageSink, provider: deps.Provider, metrics: deps.Metrics, limiter: deps.Limiter, quotaStore: deps.QuotaStore, now: deps.Now, apiKeyLimits: deps.APIKeyLimits}
	ops := chi.NewRouter()
	ops.Get("/healthz", ready.healthz)
	ops.Get("/livez", ready.healthz)
	ops.Get("/readyz", ready.readyz)
	ops.Handle("/metrics", deps.Metrics.Handler())

	router := chi.NewRouter()
	router.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
	router.Get("/healthz", ready.healthz)
	router.Get("/livez", ready.healthz)
	if deps.Config.Healthz == deps.Config.Listen {
		router.Get("/readyz", ready.readyz)
		router.Handle("/metrics", deps.Metrics.Handler())
	}
	router.Group(func(r chi.Router) {
		r.Use(handlers.authenticate)
		r.Get("/v1/models", handlers.models)
		r.Post("/v1/chat/completions", handlers.chat)
		r.Post("/v1/embeddings", handlers.embeddings)
	})

	return &Server{
		HTTP:        &http.Server{Addr: deps.Config.Listen, Handler: router, ReadHeaderTimeout: 10 * time.Second},
		Ops:         ops,
		ready:       ready,
		config:      deps.Config,
		logger:      deps.Logger,
		limiter:     deps.Limiter,
		quotaStore:  deps.QuotaStore,
		now:         deps.Now,
		apiKeyLimit: deps.APIKeyLimits,
	}
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
	server := New(Deps{Config: cfg, Logger: logger, Authenticator: authenticator, APIKeyLimits: limits})
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
		apiErr := server.HTTP.Shutdown(shutdownCtx)
		healthErr := health.Shutdown(shutdownCtx)
		if apiErr != nil {
			return apiErr
		}
		return healthErr
	}
}
