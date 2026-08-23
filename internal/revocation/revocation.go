// Package revocation backs the admin key-revocation endpoint: a revoked
// key is cut off at runtime without a config change or restart.
//
// Backends: memory (per-replica) or redis (shared; every replica honors a
// revocation within the lookup cache TTL). Both fail open on backend
// errors, matching the rate-limit/quota layers.
package revocation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nidjbs/go-ai-gateway/internal/config"
	"github.com/nidjbs/go-ai-gateway/internal/redisutil"
)

// Store is the revocation set; implementations must be concurrency-safe.
type Store interface {
	Revoke(ctx context.Context, keyID string) error
	IsRevoked(keyID string) bool // fails open on backend errors
	RevokedKeys() []string
	Close() error
}

const (
	keyPrefix     = "admin:revoked:"
	maxCachedKeys = 10_000 // local lookup cache bound; flushed when exceeded
)

// New builds a Store from the admin revocation driver config; empty
// Driver defaults to "memory".
func New(driver config.StorageDriver, cacheTTL, revokeTTL time.Duration, logger *slog.Logger) (Store, error) {
	name := driver.Driver
	if name == "" {
		name = "memory"
	}
	if cacheTTL <= 0 {
		cacheTTL = 5 * time.Second
	}
	switch name {
	case "memory":
		return NewMemory(), nil
	case "redis":
		return NewRedis(driver.Options, cacheTTL, revokeTTL, logger)
	default:
		return nil, fmt.Errorf("revocation: driver %q not registered (want memory or redis)", name)
	}
}

type memoryStore struct {
	mu   sync.RWMutex
	keys map[string]struct{}
}

// NewMemory returns an in-process revocation store.
func NewMemory() Store {
	return &memoryStore{keys: make(map[string]struct{})}
}

func (m *memoryStore) Revoke(_ context.Context, keyID string) error {
	if keyID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[keyID] = struct{}{}
	return nil
}

func (m *memoryStore) IsRevoked(keyID string) bool {
	if keyID == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.keys[keyID]
	return ok
}

func (m *memoryStore) RevokedKeys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.keys))
	for k := range m.keys {
		out = append(out, k)
	}
	return out
}

func (m *memoryStore) Close() error { return nil }

type cacheEntry struct {
	revoked   bool
	expiresAt time.Time
}

type redisStore struct {
	client    redis.UniversalClient
	cacheTTL  time.Duration
	revokeTTL time.Duration
	logger    *slog.Logger
	mu        sync.Mutex
	cache     map[string]cacheEntry
}

// NewRedis returns a revocation store backed by Redis. Lookups are cached
// locally for cacheTTL so the request hot path avoids a round trip; a
// revocation issued on another replica propagates within that TTL.
func NewRedis(opts map[string]any, cacheTTL, revokeTTL time.Duration, logger *slog.Logger) (Store, error) {
	o, err := redisutil.Parse(opts)
	if err != nil {
		return nil, fmt.Errorf("revocation redis: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &redisStore{
		client:    o.UniversalClient(),
		cacheTTL:  cacheTTL,
		revokeTTL: revokeTTL,
		logger:    logger,
		cache:     make(map[string]cacheEntry),
	}, nil
}

func (r *redisStore) Revoke(ctx context.Context, keyID string) error {
	if keyID == "" {
		return nil
	}
	key := keyPrefix + keyID
	var err error
	if r.revokeTTL > 0 {
		err = r.client.Set(ctx, key, "1", r.revokeTTL).Err()
	} else {
		err = r.client.Set(ctx, key, "1", 0).Err()
	}
	if err != nil {
		return fmt.Errorf("revocation: revoke %s: %w", keyID, err)
	}
	// Reflect the revocation locally so this replica honors it immediately.
	r.mu.Lock()
	r.cache[keyID] = cacheEntry{revoked: true, expiresAt: time.Now().Add(r.cacheTTL)}
	r.mu.Unlock()
	return nil
}

func (r *redisStore) IsRevoked(keyID string) bool {
	if keyID == "" {
		return false
	}
	now := time.Now()
	r.mu.Lock()
	if entry, ok := r.cache[keyID]; ok && now.Before(entry.expiresAt) {
		r.mu.Unlock()
		return entry.revoked
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := r.client.Get(ctx, keyPrefix+keyID).Result()
	revoked := false
	switch {
	case err == nil:
		revoked = true
	case errors.Is(err, redis.Nil):
		revoked = false
	default:
		r.logger.Warn("revocation: check failed, failing open", "key_id", keyID, "error", err)
		revoked = false
	}

	r.mu.Lock()
	if len(r.cache) >= maxCachedKeys {
		r.cache = make(map[string]cacheEntry)
	}
	r.cache[keyID] = cacheEntry{revoked: revoked, expiresAt: now.Add(r.cacheTTL)}
	r.mu.Unlock()
	return revoked
}

func (r *redisStore) RevokedKeys() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var (
		cursor uint64
		out    []string
	)
	for {
		keys, next, err := r.client.Scan(ctx, cursor, keyPrefix+"*", 100).Result()
		if err != nil {
			return out
		}
		for _, k := range keys {
			out = append(out, strings.TrimPrefix(k, keyPrefix))
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return out
}

func (r *redisStore) Close() error {
	if r.client == nil {
		return nil
	}
	return r.client.Close()
}

// Compile-time interface guards.
var (
	_ Store = (*memoryStore)(nil)
	_ Store = (*redisStore)(nil)
)
