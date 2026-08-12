package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen            string              `yaml:"listen"`
	Healthz           string              `yaml:"healthz"`
	ReadyzWaitTime    time.Duration       `yaml:"readyz_wait_time"`
	Auth              AuthConfig          `yaml:"auth"`
	Providers         map[string]Provider `yaml:"providers"`
	Aliases           map[string]Alias    `yaml:"aliases"`
	Retry             RetryConfig         `yaml:"retry"`
	Failover          FailoverConfig      `yaml:"failover"`
	Teams             []TeamConfig        `yaml:"teams"`
	readyzWaitTimeSet bool
}

func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	type raw Config
	var decoded raw
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*c = Config(decoded)
	c.readyzWaitTimeSet = hasYAMLKey(value, "readyz_wait_time")
	return nil
}

type AuthConfig struct {
	Mode     string `yaml:"mode"`
	TokenEnv string `yaml:"token_env"`
}

type Provider struct {
	Type           string        `yaml:"type"`
	APIKeyEnv      string        `yaml:"api_key_env"`
	APIKey         string        `yaml:"-"`
	BaseURL        string        `yaml:"base_url"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

type Alias struct {
	Provider  string          `yaml:"provider,omitempty"`
	Model     string          `yaml:"model,omitempty"`
	Providers []AliasProvider `yaml:"providers,omitempty"`
}

type AliasProvider struct {
	Name     string `yaml:"name"`
	Model    string `yaml:"model"`
	Priority int    `yaml:"priority"`
}

type TeamConfig struct {
	ID      string         `yaml:"id"`
	Name    string         `yaml:"name"`
	APIKeys []APIKeyConfig `yaml:"api_keys"`
}

type APIKeyConfig struct {
	ID     string    `yaml:"id"`
	Key    string    `yaml:"key"`
	Limits KeyLimits `yaml:"limits"`
}

type KeyLimits struct {
	RPS          float64 `yaml:"rps"`
	Burst        int     `yaml:"burst"`
	PredayTokens int64   `yaml:"preday_tokens"`
}

type RetryConfig struct {
	Enabled                bool          `yaml:"enabled"`
	MaxAttemptsPerProvider uint          `yaml:"max_attempts_per_provider"`
	MaxElapsedTime         time.Duration `yaml:"max_elapsed_time"`
	PerAttemptTimeout      time.Duration `yaml:"per_attempt_timeout"`
	InitialInterval        time.Duration `yaml:"initial_interval"`
	MaxInterval            time.Duration `yaml:"max_interval"`
	Multiplier             float64       `yaml:"multiplier"`
	Jitter                 float64       `yaml:"jitter"`
	RetryableStatuses      []int         `yaml:"retryable_statuses"`
	enabledSet             bool
}

type FailoverConfig struct {
	Enabled      bool `yaml:"enabled"`
	MaxProviders uint `yaml:"max_providers"`
	enabledSet   bool
}

func (c *RetryConfig) UnmarshalYAML(value *yaml.Node) error {
	type raw RetryConfig
	var decoded raw
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*c = RetryConfig(decoded)
	c.enabledSet = hasYAMLKey(value, "enabled")
	return nil
}

func (c *FailoverConfig) UnmarshalYAML(value *yaml.Node) error {
	type raw FailoverConfig
	var decoded raw
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*c = FailoverConfig(decoded)
	c.enabledSet = hasYAMLKey(value, "enabled")
	return nil
}

func hasYAMLKey(value *yaml.Node, key string) bool {
	if value.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		if value.Content[i].Value == key {
			return true
		}
	}
	return false
}

func (c RetryConfig) IsEnabled() bool { return !c.enabledSet || c.Enabled }

func (c FailoverConfig) IsEnabled() bool { return !c.enabledSet || c.Enabled }

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.resolveSecrets(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.FinalizeAPIKeys()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.Healthz == "" {
		c.Healthz = c.Listen
	}
	if !c.readyzWaitTimeSet {
		c.ReadyzWaitTime = 10 * time.Second
	}
	if c.Auth.Mode == "" {
		c.Auth.Mode = "none"
	}
	if c.Retry.MaxAttemptsPerProvider == 0 {
		c.Retry.MaxAttemptsPerProvider = 1
	}
	if c.Retry.MaxElapsedTime == 0 {
		c.Retry.MaxElapsedTime = 2 * time.Minute
	}
	if c.Retry.InitialInterval == 0 {
		c.Retry.InitialInterval = 200 * time.Millisecond
	}
	if c.Retry.MaxInterval == 0 {
		c.Retry.MaxInterval = 5 * time.Second
	}
	if c.Retry.Multiplier == 0 {
		c.Retry.Multiplier = 2
	}
	if c.Retry.Jitter == 0 {
		c.Retry.Jitter = 0.2
	}
	if c.Retry.RetryableStatuses == nil {
		c.Retry.RetryableStatuses = []int{408, 429, 500, 502, 503, 504}
	}
}

func (c *Config) resolveSecrets() error {
	for name, provider := range c.Providers {
		if provider.APIKeyEnv == "" {
			continue
		}
		value, ok := os.LookupEnv(provider.APIKeyEnv)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("provider %q: api_key_env %q is unset or empty", name, provider.APIKeyEnv)
		}
		provider.APIKey = value
		c.Providers[name] = provider
	}
	return nil
}

func (c *Config) Validate() error {
	for _, address := range []struct{ name, value string }{{"listen", c.Listen}, {"healthz", c.Healthz}} {
		if _, _, err := net.SplitHostPort(address.value); err != nil {
			return fmt.Errorf("invalid %s address %q: %w", address.name, address.value, err)
		}
	}
	if c.Auth.Mode != "none" && c.Auth.Mode != "static" && c.Auth.Mode != "api-key" {
		return fmt.Errorf("auth.mode %q is unsupported", c.Auth.Mode)
	}
	if c.Auth.Mode == "static" && strings.TrimSpace(c.Auth.TokenEnv) == "" {
		return errors.New("auth.token_env is required when auth.mode is static")
	}
	if c.Auth.Mode == "api-key" {
		if err := c.validateTeams(); err != nil {
			return err
		}
	}
	if c.Retry.Multiplier <= 0 {
		return errors.New("retry.multiplier must be greater than zero")
	}
	if c.Retry.Jitter < 0 || c.Retry.Jitter >= 1 {
		return errors.New("retry.jitter must be greater than or equal to zero and less than one")
	}
	if len(c.Providers) == 0 {
		return errors.New("at least one provider is required")
	}
	for name, provider := range c.Providers {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("provider %q: base_url is required", name)
		}
		if provider.Type != "" && provider.Type != "openai" && provider.Type != "anthropic" {
			return fmt.Errorf("provider %q: type %q is unsupported", name, provider.Type)
		}
	}
	if len(c.Aliases) == 0 {
		return errors.New("at least one alias is required")
	}
	for alias, definition := range c.Aliases {
		providers := definition.Providers
		if len(providers) == 0 && definition.Provider != "" {
			providers = []AliasProvider{{Name: definition.Provider, Model: definition.Model}}
		}
		if len(providers) == 0 {
			return fmt.Errorf("alias %q: provider is required", alias)
		}
		for i, candidate := range providers {
			if _, ok := c.Providers[candidate.Name]; !ok {
				return fmt.Errorf("alias %q providers[%d]: unknown provider %q", alias, i, candidate.Name)
			}
			if strings.TrimSpace(candidate.Model) == "" {
				return fmt.Errorf("alias %q providers[%d]: model is required", alias, i)
			}
		}
	}
	return nil
}

func (a Alias) NormalizedProviders() []AliasProvider {
	if len(a.Providers) > 0 {
		out := append([]AliasProvider(nil), a.Providers...)
		return out
	}
	if a.Provider == "" {
		return nil
	}
	return []AliasProvider{{Name: a.Provider, Model: a.Model}}
}

func (c *Config) AliasNames() []string {
	names := make([]string, 0, len(c.Aliases))
	for name := range c.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (c *Config) TeamsEnabled() bool {
	return c.Auth.Mode == "api-key"
}

func (c *Config) validateTeams() error {
	if len(c.Teams) == 0 {
		return errors.New("auth.mode is api-key but no teams are configured")
	}
	teamIDs := make(map[string]struct{}, len(c.Teams))
	apiKeyIDs := make(map[string]struct{})
	rawKeys := make(map[string]struct{})
	for i, team := range c.Teams {
		id := strings.TrimSpace(team.ID)
		if id == "" {
			return fmt.Errorf("teams[%d]: id is required", i)
		}
		if !validIdentifier(id) {
			return fmt.Errorf("teams[%d]: id %q contains invalid characters", i, id)
		}
		if _, ok := teamIDs[id]; ok {
			return fmt.Errorf("teams[%d]: duplicate team id %q", i, id)
		}
		teamIDs[id] = struct{}{}
		if len(team.APIKeys) == 0 {
			return fmt.Errorf("teams[%d] %q: api_keys must not be empty", i, id)
		}
		for j, key := range team.APIKeys {
			keyID := strings.TrimSpace(key.ID)
			if keyID == "" {
				return fmt.Errorf("teams[%d].api_keys[%d]: id is required", i, j)
			}
			if !validIdentifier(keyID) {
				return fmt.Errorf("teams[%d].api_keys[%d]: id %q contains invalid characters", i, j, keyID)
			}
			if _, ok := apiKeyIDs[keyID]; ok {
				return fmt.Errorf("teams[%d].api_keys[%d]: duplicate api key id %q", i, j, keyID)
			}
			apiKeyIDs[keyID] = struct{}{}
			raw := strings.TrimSpace(key.Key)
			if raw == "" {
				return fmt.Errorf("teams[%d].api_keys[%d]: key is required", i, j)
			}
			if _, ok := rawKeys[raw]; ok {
				return fmt.Errorf("teams[%d].api_keys[%d]: duplicate api key", i, j)
			}
			rawKeys[raw] = struct{}{}
			if key.Limits.RPS < 0 {
				return fmt.Errorf("teams[%d].api_keys[%d]: limits.rps must be non-negative", i, j)
			}
			if key.Limits.Burst < 0 {
				return fmt.Errorf("teams[%d].api_keys[%d]: limits.burst must be non-negative", i, j)
			}
			if key.Limits.PredayTokens < 0 {
				return fmt.Errorf("teams[%d].api_keys[%d]: limits.preday_tokens must be non-negative", i, j)
			}
		}
	}
	return nil
}

func (c *Config) FinalizeAPIKeys() {
	for i, team := range c.Teams {
		for j, key := range team.APIKeys {
			if key.Limits.RPS > 0 && key.Limits.Burst == 0 {
				key.Limits.Burst = int(key.Limits.RPS)
				if key.Limits.Burst < 1 {
					key.Limits.Burst = 1
				}
				team.APIKeys[j] = key
			}
		}
		c.Teams[i] = team
	}
}

func validIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
