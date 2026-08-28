package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/nidjbs/go-ai-gateway/internal/auth"
	"github.com/nidjbs/go-ai-gateway/internal/circuitbreaker"
	"github.com/nidjbs/go-ai-gateway/internal/config"
	"github.com/nidjbs/go-ai-gateway/internal/dlp"
	"github.com/nidjbs/go-ai-gateway/internal/provider"
	"github.com/nidjbs/go-ai-gateway/internal/revocation"
)

// runtime is one immutable configuration snapshot. The handler and server
// read hot-reloadable state exclusively through runtime; reload builds a new
// snapshot and atomically swaps it in. Slow-path components (limiter, quota
// store, usage sink) stay on the handler and are reused across reloads so
// counters and latency observations survive.
type runtime struct {
	configPath    string
	config        *config.Config
	authenticator auth.Authenticator
	provider      *provider.Client
	apiKeyLimits  map[string]config.KeyLimits
	breaker       circuitbreaker.Breaker
	dlpDetector   *dlp.Detector
	revoker       revocation.Store
}

// rt returns the currently-active runtime snapshot.
func (h handler) rt() *runtime { return h.rtPtr.Load() }

// buildRuntime derives the hot-reloadable state from a loaded config. Shared
// between startup and reload so both paths behave identically. Injected
// deps (Authenticator/Provider/APIKeyLimits/Breaker) win over config-derived
// defaults so tests and embedders keep their overrides.
func buildRuntime(configPath string, cfg *config.Config, deps Deps) (*runtime, error) {
	authenticator := deps.Authenticator
	if authenticator == nil {
		var err error
		authenticator, err = auth.New(cfg.Auth, cfg.Teams)
		if err != nil {
			return nil, err
		}
	}
	var limits map[string]config.KeyLimits
	if deps.APIKeyLimits != nil {
		limits = deps.APIKeyLimits
	} else {
		limits = make(map[string]config.KeyLimits, len(cfg.Teams))
		for _, team := range cfg.Teams {
			for _, key := range team.APIKeys {
				limits[key.ID] = key.Limits
			}
		}
	}
	providerClient := deps.Provider
	if providerClient == nil {
		providerClient = provider.NewClient()
	}
	if cfg.Tracing.Enabled {
		for _, ptype := range providerClient.RegisteredTypes() {
			existing := providerClient.HTTPClient(ptype)
			providerClient.SetHTTPClient(ptype, &http.Client{
				Transport: otelhttp.NewTransport(existing.Transport),
				Timeout:   existing.Timeout,
			})
		}
	}
	var breaker circuitbreaker.Breaker
	if deps.Breaker != nil {
		breaker = deps.Breaker
	} else if cfg.CircuitBreaker.Enabled {
		breaker = circuitbreaker.New(circuitbreaker.Config{
			FailureThreshold:         cfg.CircuitBreaker.FailureThreshold,
			OpenDuration:             cfg.CircuitBreaker.OpenDuration,
			HalfOpenMaxRequests:      cfg.CircuitBreaker.HalfOpenMaxRequests,
			HalfOpenSuccessThreshold: cfg.CircuitBreaker.HalfOpenSuccessThreshold,
			ErrorRate:                cfg.CircuitBreaker.ErrorRate,
			Window:                   cfg.CircuitBreaker.Window,
			MinSamples:               cfg.CircuitBreaker.MinSamples,
		})
	} else {
		breaker = circuitbreaker.Noop{}
	}
	dlpDetector, err := dlp.New(dlp.Config{
		Enabled:   cfg.DLP.Enabled,
		Mode:      cfg.DLP.Mode,
		MaskText:  cfg.DLP.MaskText,
		Patterns:  cfg.DLP.Patterns,
		CarrySize: cfg.DLP.CarrySize,
	})
	if err != nil {
		return nil, fmt.Errorf("dlp: %w", err)
	}
	// Revoker backs the admin surface; nil disables revocation endpoints.
	var revoker revocation.Store
	if cfg.Admin.Enabled {
		token := strings.TrimSpace(os.Getenv(cfg.Admin.TokenEnv))
		if token == "" {
			return nil, fmt.Errorf("admin.token_env %q is unset or empty", cfg.Admin.TokenEnv)
		}
		logger := deps.Logger
		if logger == nil {
			logger = slog.Default()
		}
		revoker, err = revocation.New(cfg.Admin.Revocation, cfg.Admin.CacheTTL, cfg.Admin.RevokeTTL, logger)
		if err != nil {
			return nil, fmt.Errorf("admin revocation: %w", err)
		}
	}
	return &runtime{
		configPath:    configPath,
		config:        cfg,
		authenticator: authenticator,
		provider:      providerClient,
		apiKeyLimits:  limits,
		breaker:       breaker,
		dlpDetector:   dlpDetector,
		revoker:       revoker,
	}, nil
}
