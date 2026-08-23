package ratelimit

import (
	"github.com/nidjbs/go-ai-gateway/internal/redisutil"
)

// Redis driver registration. Both Limiter and QuotaStore share a single
// "redis" driver so the operator only configures one connection block.

func init() {
	LimiterRegistry.Register("redis", newRedisLimiter)
	QuotaRegistry.Register("redis", newRedisQuotaStore)
}

// parseRedisOptions forwards to redisutil.Parse, keeping the historical
// error message for the missing-addr case.
func parseRedisOptions(opts map[string]any) (redisutil.Options, error) {
	return redisutil.Parse(opts)
}

// newRedisLimiter is the factory entry in LimiterRegistry.
func newRedisLimiter(opts map[string]any) (Limiter, error) {
	o, err := parseRedisOptions(opts)
	if err != nil {
		return nil, err
	}
	return NewRedisLimiter(o.UniversalClient()), nil
}

// newRedisQuotaStore is the factory entry in QuotaRegistry.
func newRedisQuotaStore(opts map[string]any) (QuotaStore, error) {
	o, err := parseRedisOptions(opts)
	if err != nil {
		return nil, err
	}
	return NewRedisQuotaStore(o.UniversalClient()), nil
}

// NewRedisPinger returns a readiness probe that pings the Redis server
// described by opts. The probe is safe for concurrent use and bounds each
// ping to one second so readyz never stalls on a dead backend. Gateway
// startup invokes the probe once to fail fast on a misconfigured cluster
// instead of silently degrading to fail-open at request time.
func NewRedisPinger(opts map[string]any) (func() error, error) {
	return redisutil.NewPinger(opts)
}
