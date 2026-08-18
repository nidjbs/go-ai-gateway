package ratelimit

import (
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// redisClientFor builds a *redis.Client connected to the in-process
// miniredis instance. Used by the Redis-driver tests so they don't need a
// running Redis server.
//
// The returned client is closed when the *miniredis.Miniredis returned by
// miniredis.RunT is torn down by t.Cleanup.
func redisClientFor(s *miniredis.Miniredis) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: s.Addr(),
	})
}
