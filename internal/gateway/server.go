package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"example.com/light-llm-gateway/internal/auth"
	"example.com/light-llm-gateway/internal/config"
	"example.com/light-llm-gateway/internal/provider"
	"example.com/light-llm-gateway/internal/usage"
)

type Deps struct {
	Config        *config.Config
	Logger        *slog.Logger
	Authenticator auth.Authenticator
	UsageSink     usage.Sink
	Provider      *provider.Client
}

type Server struct {
	HTTP   *http.Server
	config *config.Config
	logger *slog.Logger
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

	handlers := handler{config: deps.Config, logger: deps.Logger, authenticator: deps.Authenticator, usageSink: deps.UsageSink, provider: deps.Provider}
	router := chi.NewRouter()
	router.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
	router.Get("/healthz", handlers.healthz)
	router.Group(func(r chi.Router) {
		r.Use(handlers.authenticate)
		r.Get("/v1/models", handlers.models)
		r.Post("/v1/chat/completions", handlers.chat)
		r.Post("/v1/embeddings", handlers.embeddings)
	})

	return &Server{
		HTTP:   &http.Server{Addr: deps.Config.Listen, Handler: router, ReadHeaderTimeout: 10 * time.Second},
		config: deps.Config,
		logger: deps.Logger,
	}
}

func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	authenticator, err := auth.New(cfg.Auth)
	if err != nil {
		return err
	}
	server := New(Deps{Config: cfg, Logger: logger, Authenticator: authenticator})
	health := &http.Server{
		Addr:              cfg.Healthz,
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		ReadHeaderTimeout: 10 * time.Second,
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
