// Package redisutil builds go-redis clients for every Redis-backed gateway
// backend (rate limits, quotas, guardrail tracker, key revocation), with
// standalone, Sentinel, and Cluster failover support.
package redisutil

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Options mirrors the keys accepted under any `options:` block: addr (or
// comma-separated addrs for Cluster), password, db, tls, dial_timeout,
// read_timeout, sentinel_addrs, master_name, sentinel_password.
type Options struct {
	Addrs         []string
	Password      string
	DB            int
	TLS           bool
	DialTimeout   time.Duration
	ReadTimeout   time.Duration
	SentinelAddrs []string
	MasterName    string
	SentinelPass  string
}

// Parse builds Options from a raw options map.
func Parse(opts map[string]any) (Options, error) {
	if opts == nil {
		opts = map[string]any{}
	}
	o := Options{
		Addrs:       splitList(stringOption(opts, "addr", "")),
		Password:    stringOption(opts, "password", ""),
		DB:          intOption(opts, "db", 0),
		TLS:         boolOption(opts, "tls", false),
		DialTimeout: durationOption(opts, "dial_timeout", 2*time.Second),
		ReadTimeout: durationOption(opts, "read_timeout", 250*time.Millisecond),
	}
	if s := stringOption(opts, "sentinel_addrs", ""); s != "" {
		o.SentinelAddrs = splitList(s)
		o.MasterName = stringOption(opts, "master_name", "")
		o.SentinelPass = stringOption(opts, "sentinel_password", "")
		if o.MasterName == "" {
			return Options{}, errors.New("redis: master_name is required when sentinel_addrs is set")
		}
	}
	if len(o.Addrs) == 0 && len(o.SentinelAddrs) == 0 {
		return Options{}, errors.New("redis: addr (or sentinel_addrs) is required")
	}
	return o, nil
}

// UniversalClient returns the go-redis client for the mode implied by the
// options: single node, Sentinel (master_name set), or Cluster.
func (o Options) UniversalClient() redis.UniversalClient {
	rcfg := &redis.UniversalOptions{
		Addrs:            o.addrs(),
		MasterName:       o.MasterName,
		Password:         o.Password,
		SentinelPassword: o.SentinelPass,
		DB:               o.DB,
		DialTimeout:      o.DialTimeout,
		ReadTimeout:      o.ReadTimeout,
		WriteTimeout:     o.ReadTimeout,
	}
	if o.TLS {
		rcfg.TLSConfig = &tls.Config{}
	}
	return redis.NewUniversalClient(rcfg)
}

// NewPinger returns a readiness probe pinging the Redis backend.
func NewPinger(opts map[string]any) (func() error, error) {
	o, err := Parse(opts)
	if err != nil {
		return nil, err
	}
	client := o.UniversalClient()
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := client.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("redis ping: %w", err)
		}
		return nil
	}, nil
}

func (o Options) addrs() []string {
	if len(o.SentinelAddrs) > 0 {
		return o.SentinelAddrs
	}
	return o.Addrs
}

func splitList(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func stringOption(opts map[string]any, key, def string) string {
	if opts == nil {
		return def
	}
	if v, ok := opts[key].(string); ok && v != "" {
		return v
	}
	return def
}

func intOption(opts map[string]any, key string, def int) int {
	if opts == nil {
		return def
	}
	switch v := opts[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}

func boolOption(opts map[string]any, key string, def bool) bool {
	if opts == nil {
		return def
	}
	if v, ok := opts[key].(bool); ok {
		return v
	}
	return def
}

func durationOption(opts map[string]any, key string, def time.Duration) time.Duration {
	if opts == nil {
		return def
	}
	switch v := opts[key].(type) {
	case string:
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	case int:
		return time.Duration(v)
	case int64:
		return time.Duration(v)
	case float64:
		return time.Duration(v)
	}
	return def
}
