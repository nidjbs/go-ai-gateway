package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/nidjbs/go-ai-gateway/internal/apierr"
	"github.com/nidjbs/go-ai-gateway/internal/config"
)

// reloadRequest optionally overrides the config file path; empty keeps the
// path the gateway was started with.
type reloadRequest struct {
	Path string `json:"path"`
}

// reloadConfig reloads the config file, rebuilds the hot-reloadable state and
// atomically swaps it in. A failed parse/validation leaves the previous config
// untouched; storage-driver changes require a restart.
func (h handler) reloadConfig(w http.ResponseWriter, r *http.Request) {
	old := h.rt()
	path := old.configPath
	if r.Body != nil {
		var req reloadRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && err != io.EOF {
			apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", "invalid reload request body")
			return
		}
		if strings.TrimSpace(req.Path) != "" {
			path = strings.TrimSpace(req.Path)
		}
	}
	newCfg, err := config.Load(path)
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_config", "invalid_request_error", "failed to load config: "+err.Error())
		return
	}
	if err := checkDriversStable(old.config, newCfg); err != nil {
		apierr.Write(w, http.StatusConflict, "driver_change_requires_restart", "server_error", err.Error())
		return
	}
	next, err := buildRuntime(path, newCfg, Deps{Logger: h.logger})
	if err != nil {
		apierr.Write(w, http.StatusBadRequest, "invalid_config", "invalid_request_error", "failed to build runtime: "+err.Error())
		return
	}
	h.rtPtr.Store(next)
	h.logger.Info("config reloaded", "config_path", path)
	writeJSON(w, http.StatusOK, map[string]any{"status": "reloaded", "config_path": path})
}

// checkDriversStable rejects reloads that change a storage driver, since those
// backends keep live state (rate-limit counters, quota) that a swap would drop.
func checkDriversStable(old, new *config.Config) error {
	drivers := []struct {
		name    string
		old, be config.StorageDriver
	}{
		{"rate_limit", old.RateLimit, new.RateLimit},
		{"quota", old.Quota, new.Quota},
		{"usage", old.Usage, new.Usage},
		{"admin.revocation", old.Admin.Revocation, new.Admin.Revocation},
	}
	for _, d := range drivers {
		if d.old.Driver != d.be.Driver {
			return errors.New(d.name + " driver change requires restart")
		}
	}
	return nil
}
