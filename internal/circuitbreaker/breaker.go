// Package circuitbreaker provides a per-key circuit breaker. The breaker is keyed
// by an opaque string and is safe for concurrent use.
//
// State machine: Closed (failures trip open) → Open (rejects) → Half-Open (probes) → Closed.
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
var ErrOpen = errors.New("circuit breaker is open")

// Config controls breaker behavior. Zero values fall back to safe defaults.
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
	// ErrorRate is the minimum failure ratio over the sliding Window that trips
	// the breaker, once at least MinSamples samples exist. Must be in (0,1].
	ErrorRate float64
	// Window is the sliding window over which error rate is measured. Must be > 0.
	Window time.Duration
	// MinSamples is the minimum sample count in Window before error rate may trip. Must be ≥ 1.
	MinSamples int
}

// NewConfig returns Config populated with sensible defaults.
func NewConfig() Config {
	return Config{
		FailureThreshold:         10,
		OpenDuration:             30 * time.Second,
		HalfOpenMaxRequests:      1,
		HalfOpenSuccessThreshold: 1,
		ErrorRate:                0.5,
		Window:                   60 * time.Second,
		MinSamples:               10,
	}
}

// Breaker is the public interface; implementations must be safe for concurrent use.
type Breaker interface {
	// Allow reports whether a call is permitted; returns ErrOpen when open.
	Allow(key string, now time.Time) error
	// Record reports the outcome of a call; the breaker updates its state machine.
	Record(key string, now time.Time, err error)
}

// sample is one recorded outcome inside the sliding error-rate window.
type sample struct {
	at     time.Time
	failed bool
}

// entry holds the state for a single key.
type entry struct {
	mu sync.Mutex

	state        State
	failures     int
	openedAt     time.Time
	halfOpenBusy int
	halfOpenSucc int
	samples      []sample // ascending by at; pruned on every Record
}

// New returns an in-process Breaker. State is held in memory only.
func New(cfg Config) Breaker {
	if cfg.FailureThreshold < 1 {
		cfg.FailureThreshold = 10
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
	if cfg.ErrorRate <= 0 || cfg.ErrorRate > 1 {
		cfg.ErrorRate = 0.5
	}
	if cfg.Window <= 0 {
		cfg.Window = 60 * time.Second
	}
	if cfg.MinSamples < 1 {
		cfg.MinSamples = 10
	}
	return &memoryBreaker{cfg: cfg, entries: make(map[string]*entry)}
}

type memoryBreaker struct {
	cfg     Config
	mu      sync.Mutex
	entries map[string]*entry
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
		// Record every outcome so the window stays current even on success.
		e.samples = append(e.samples, sample{at: now, failed: !success})
		e.prune(now, b.cfg.Window)
		if success {
			e.failures = 0
			return
		}
		e.failures++
		if e.failures >= b.cfg.FailureThreshold || e.rateTripped(b.cfg.MinSamples, b.cfg.ErrorRate) {
			e.tripOpen(now)
		}
	case StateHalfOpen:
		e.halfOpenBusy--
		if !success {
			e.tripOpen(now)
			return
		}
		e.halfOpenSucc++
		if e.halfOpenSucc >= b.cfg.HalfOpenSuccessThreshold {
			e.state = StateClosed
			e.failures = 0
			e.samples = e.samples[:0]
			e.halfOpenBusy = 0
			e.halfOpenSucc = 0
		}
	case StateOpen:
		// Records that arrive while open are ignored: the request was not
		// permitted by Allow, so it should not have been attempted. This
		// guards against double-counting after a failed failover.
	}
}

// tripOpen transitions to Open and resets counters so Half-Open starts clean.
func (e *entry) tripOpen(now time.Time) {
	e.state = StateOpen
	e.openedAt = now
	e.failures = 0
	e.samples = e.samples[:0]
	e.halfOpenBusy = 0
	e.halfOpenSucc = 0
}

// prune drops samples older than window. Samples exactly window old are kept.
func (e *entry) prune(now time.Time, window time.Duration) {
	i := 0
	for i < len(e.samples) && now.Sub(e.samples[i].at) > window {
		i++
	}
	if i > 0 {
		e.samples = e.samples[i:]
	}
}

// rateTripped reports whether the window failure ratio meets errRate with
// at least minSamples samples. Inclusive boundary: ratio >= errRate trips.
func (e *entry) rateTripped(minSamples int, errRate float64) bool {
	if len(e.samples) < minSamples {
		return false
	}
	failed := 0
	for _, s := range e.samples {
		if s.failed {
			failed++
		}
	}
	return float64(failed)/float64(len(e.samples)) >= errRate
}
