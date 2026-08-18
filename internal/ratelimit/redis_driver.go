package ratelimit

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis driver registration. Both Limiter and QuotaStore share a single
// "redis" driver so the operator only configures one connection block.

func init() {
	LimiterRegistry.Register("redis", newRedisLimiter)
	QuotaRegistry.Register("redis", newRedisQuotaStore)
}

// redisOptions captures the fields the redis driver accepts under
// `rate_limit.options` / `quota.options`. It deliberately mirrors the
// well-known go-redis Options struct so anyone familiar with that client
// recognises the keys (addr / password / db / tls / dial_timeout).
type redisOptions struct {
	addr        string
	password    string
	db          int
	tls         bool
	dialTimeout time.Duration
	readTimeout time.Duration
}

func parseRedisOptions(opts map[string]any) (redisOptions, error) {
	if opts == nil {
		opts = map[string]any{}
	}
	addr := stringOption(opts, "addr", "127.0.0.1:6379")
	if addr == "" {
		return redisOptions{}, errors.New("redis driver: addr is required")
	}
	return redisOptions{
		addr:        addr,
		password:    stringOption(opts, "password", ""),
		db:          intOption(opts, "db", 0),
		tls:         boolOption(opts, "tls", false),
		dialTimeout: durationOption(opts, "dial_timeout", 2*time.Second),
		readTimeout: durationOption(opts, "read_timeout", 250*time.Millisecond),
	}, nil
}

func (o redisOptions) client() *redis.Client {
	rcfg := &redis.Options{
		Addr:         o.addr,
		Password:     o.password,
		DB:           o.db,
		DialTimeout:  o.dialTimeout,
		ReadTimeout:  o.readTimeout,
		WriteTimeout: o.readTimeout,
	}
	if o.tls {
		rcfg.TLSConfig = &tls.Config{}
	}
	return redis.NewClient(rcfg)
}

// newRedisLimiter is the factory entry in LimiterRegistry.
func newRedisLimiter(opts map[string]any) (Limiter, error) {
	o, err := parseRedisOptions(opts)
	if err != nil {
		return nil, err
	}
	return NewRedisLimiter(o.client()), nil
}

// newRedisQuotaStore is the factory entry in QuotaRegistry.
func newRedisQuotaStore(opts map[string]any) (QuotaStore, error) {
	o, err := parseRedisOptions(opts)
	if err != nil {
		return nil, err
	}
	return NewRedisQuotaStore(o.client()), nil
}

// pingRedis verifies connectivity at boot so a misconfigured cluster fails
// fast rather than silently degrading to fail-open at request time.
func pingRedis(client *redis.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	return nil
}
