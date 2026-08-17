// Package circuitbreaker provides a per-key circuit breaker used by the
// gateway to short-circuit calls to a failing provider+upstream pair.
//
// The breaker is keyed by an opaque string (callers typically use
// "provider_name|base_url" so that two upstreams fronted by the same provider
// type are tracked independently). It is safe for concurrent use.
//
// State machine:
//
//	Closed   - requests pass through. Consecutive failures ≥ FailureThreshold
//	           open the breaker.
//	Open     - requests are rejected with ErrOpen. After OpenDuration elapses
//	           the breaker transitions to Half-Open.
//	Half-Open - up to HalfOpenMaxRequests probe requests pass through. The
//	           first failure re-opens the breaker (resetting OpenDuration);
//	           HalfOpenSuccessThreshold consecutive successes close it.
package circuitbreaker

import (
	"errors"
	"sync"
	"time"
)

// State identifies the current state of a breaker entry.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// ErrOpen is returned by Allow when the breaker is open and rejecting calls.
// Callers should treat it as a retryable, non-counted failure: it is not the
// fault of the upstream, just a policy decision.
var ErrOpen = errors.New("circuit breaker is open")

// Config controls breaker behavior. Zero values fall back to safe defaults
// applied by NewConfig.
type Config struct {
	// FailureThreshold is the number of consecutive failures in Closed state
	// that trip the breaker open. Must be ≥ 1.
	FailureThreshold int
	// OpenDuration is how long the breaker stays Open before transitioning to
	// Half-Open. Must be > 0.
	OpenDuration time.Duration
	// HalfOpenMaxRequests caps the number of probe requests permitted while
	// in Half-Open. Must be ≥ 1.
	HalfOpenMaxRequests int
	// HalfOpenSuccessThreshold is the number of consecutive probe successes
	// required to close the breaker. Must be ≥ 1.
	HalfOpenSuccessThreshold int
}

// NewConfig returns Config populated with sensible defaults.
func NewConfig() Config {
	return Config{
		FailureThreshold:          5,
		OpenDuration:              30 * time.Second,
		HalfOpenMaxRequests:       1,
		HalfOpenSuccessThreshold:  1,
	}
}

// Breaker is the public interface. Implementations must be safe for concurrent
// use.
type Breaker interface {
	// Allow reports whether a call to the given key is permitted now. When
	// the breaker is open it returns ErrOpen along with the time remaining
	// until the next Half-Open probe window opens.
	Allow(key string, now time.Time) error
	// Record records the outcome of a call to key. err == nil indicates
	// success. The breaker updates its internal state machine based on the
	// current state and the outcome.
	Record(key string, now time.Time, err error)
}

// entry holds the state for a single key.
type entry struct {
	mu sync.Mutex

	state         State
	failures      int
	openedAt      time.Time
	halfOpenBusy  int
	halfOpenSucc  int
}

// New returns an in-process Breaker keyed by string. State is held in memory
// only; restarting the gateway resets every breaker.
func New(cfg Config) Breaker {
	if cfg.FailureThreshold < 1 {
		cfg.FailureThreshold = 5
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = 30 * time.Second
	}
	if cfg.HalfOpenMaxRequests < 1 {
		cfg.HalfOpenMaxRequests = 1
	}
	if cfg.HalfOpenSuccessThreshold < 1 {
		cfg.HalfOpenSuccessThreshold = 1
	}
	return &memoryBreaker{cfg: cfg, entries: make(map[string]*entry)}
}

type memoryBreaker struct {
	cfg     Config
	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time
}

func (b *memoryBreaker) entryFor(key string) *entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[key]
	if !ok {
		e = &entry{state: StateClosed}
		b.entries[key] = e
	}
	return e
}

func (b *memoryBreaker) Allow(key string, now time.Time) error {
	e := b.entryFor(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	switch e.state {
	case StateClosed:
		return nil
	case StateOpen:
		if now.Sub(e.openedAt) >= b.cfg.OpenDuration {
			e.state = StateHalfOpen
			e.halfOpenBusy = 0
			e.halfOpenSucc = 0
			// fall through to half-open handling
		} else {
			return ErrOpen
		}
		fallthrough
	case StateHalfOpen:
		if e.halfOpenBusy >= b.cfg.HalfOpenMaxRequests {
			return ErrOpen
		}
		e.halfOpenBusy++
		return nil
	}
	return nil
}

func (b *memoryBreaker) Record(key string, now time.Time, err error) {
	e := b.entryFor(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	success := err == nil
	switch e.state {
	case StateClosed:
		if success {
			e.failures = 0
			return
		}
		e.failures++
		if e.failures >= b.cfg.FailureThreshold {
			e.state = StateOpen
			e.openedAt = now
			e.failures = 0
		}
	case StateHalfOpen:
		e.halfOpenBusy--
		if !success {
			e.state = StateOpen
			e.openedAt = now
			e.halfOpenBusy = 0
			e.halfOpenSucc = 0
			return
		}
		e.halfOpenSucc++
		if e.halfOpenSucc >= b.cfg.HalfOpenSuccessThreshold {
			e.state = StateClosed
			e.failures = 0
			e.halfOpenBusy = 0
			e.halfOpenSucc = 0
		}
	case StateOpen:
		// Records that arrive while open are ignored: the request was not
		// permitted by Allow, so it should not have been attempted. This
		// guards against double-counting after a failed failover.
	}
}