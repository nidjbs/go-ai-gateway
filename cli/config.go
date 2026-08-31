package main

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the CLI's own config: where the gateway lives and how to auth.
// It is unrelated to the gateway's server config file.
type Config struct {
	GatewayURL string `yaml:"gateway_url"`
	// AdminURL is the gateway's ops/healthz port where admin endpoints live;
	// defaults to GatewayURL when empty.
	AdminURL     string `yaml:"admin_url"`
	APIKey       string `yaml:"api_key"`
	AdminToken   string `yaml:"admin_token"`
	DefaultAlias string `yaml:"default_alias"`
	// FileRoots are the directories read_file/write_file may access in agent
	// sessions; empty means the working directory at session start.
	FileRoots []string `yaml:"file_roots"`
	// WriteConfirm controls write confirmation: auto (TTY prompt, non-TTY deny),
	// always (prompt, fail on non-TTY), never (skip confirmation inside roots).
	WriteConfirm string `yaml:"write_confirm"`
}

// gwStateDir returns the CLI's state directory: config, prompts, and the
// gateway process files gw up/down manage. Overridable via GW_STATE_DIR.
func gwStateDir() string {
	if v := os.Getenv("GW_STATE_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gw")
}

// configPath returns $GW_CONFIG or <state>/config.yaml.
func configPath() (string, error) {
	if p := os.Getenv("GW_CONFIG"); p != "" {
		return p, nil
	}
	return filepath.Join(gwStateDir(), "config.yaml"), nil
}

// promptsDir returns the custom prompt directory (or $GW_PROMPTS_DIR).
func promptsDir() string {
	if p := os.Getenv("GW_PROMPTS_DIR"); p != "" {
		return p
	}
	return filepath.Join(gwStateDir(), "prompts")
}

// sessionsDir returns where session logs are written ($GW_SESSION_DIR or
// <state>/sessions).
func sessionsDir() string {
	if v := os.Getenv("GW_SESSION_DIR"); v != "" {
		return v
	}
	return filepath.Join(gwStateDir(), "sessions")
}

// loadConfig reads the CLI config file (absent file → defaults) then applies
// environment overrides: GW_GATEWAY_URL, GW_API_KEY, GW_ADMIN_TOKEN, GW_ALIAS.
func loadConfig() (*Config, error) {
	cfg := &Config{GatewayURL: "http://127.0.0.1:8080", DefaultAlias: "chat"}
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if v := os.Getenv("GW_GATEWAY_URL"); v != "" {
		cfg.GatewayURL = v
	}
	if v := os.Getenv("GW_ADMIN_URL"); v != "" {
		cfg.AdminURL = v
	}
	if v := os.Getenv("GW_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("GW_ADMIN_TOKEN"); v != "" {
		cfg.AdminToken = v
	}
	if v := os.Getenv("GW_ALIAS"); v != "" {
		cfg.DefaultAlias = v
	}
	if v := os.Getenv("GW_FILE_ROOTS"); v != "" {
		cfg.FileRoots = filepath.SplitList(v)
	}
	return cfg, nil
}
