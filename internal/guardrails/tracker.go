package guardrails

import (
	"sync"
	"time"
)

// Tracker is the per-key injection-frequency counter the middleware consults
// before letting a request through. Implementations must be safe for
// concurrent use and tolerate transient backend failures without blocking
// the request hot path indefinitely.
type Tracker interface {
	// Record increments the running counter for keyID and reports whether the
	// key has crossed the configured threshold (i.e. entered the penalty
	// window). It returns false on every increment before the threshold.
	Record(keyID string, now time.Time) bool
	// IsBlocked reports whether keyID is currently inside its penalty window.
	IsBlocked(keyID string, now time.Time) bool
	// PenaltyRemaining returns the time left on the active penalty for keyID,
	// or 0 when the key is not blocked.
	PenaltyRemaining(keyID string, now time.Time) time.Duration
	// Reset clears all state for keyID. Used by tests and operational tooling.
	Reset(keyID string)
	// ActiveBlocks returns the number of keys currently in penalty; useful
	// for monitoring.
	ActiveBlocks(now time.Time) int
}

// InjectionTracker is the in-process Tracker implementation.
//
// Repeated detections inside a short window escalate to a penalty, making
// automated attacks expensive while tolerating isolated false positives.
type InjectionTracker struct {
	mu       sync.Mutex
	windows  map[string]*attackWindow
	maxCount int
	window   time.Duration
	penalty  time.Duration
}

type attackWindow struct {
	count   int
	resetAt time.Time
	blocked bool
}

// TrackerConfig configures the in-process tracker. Distributed drivers may
// read the same fields; the gateway treats them as policy regardless of
// where state lives.
type TrackerConfig struct {
	MaxAttempts int
	Window      time.Duration
	Penalty     time.Duration
}

// DefaultTrackerConfig returns sensible defaults: 3 hits in 1 minute triggers a 30-second block.
func DefaultTrackerConfig() TrackerConfig {
	return TrackerConfig{
		MaxAttempts: 3,
		Window:      time.Minute,
		Penalty:     30 * time.Second,
	}
}

// NewInjectionTracker constructs the memory-backed tracker.
func NewInjectionTracker(cfg TrackerConfig) *InjectionTracker {
	return &InjectionTracker{
		windows:  make(map[string]*attackWindow),
		maxCount: cfg.MaxAttempts,
		window:   cfg.Window,
		penalty:  cfg.Penalty,
	}
}

// Compile-time guarantee that InjectionTracker satisfies Tracker.
var _ Tracker = (*InjectionTracker)(nil)

func (t *InjectionTracker) Record(keyID string, now time.Time) bool {
	if keyID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	w, ok := t.windows[keyID]
	if !ok || now.After(w.resetAt) {
		t.windows[keyID] = &attackWindow{count: 1, resetAt: now.Add(t.window)}
		return false
	}

	w.count++
	if w.count >= t.maxCount {
		w.blocked = true
		w.resetAt = now.Add(t.penalty)
		w.count = 0
		return true
	}
	return false
}

func (t *InjectionTracker) IsBlocked(keyID string, now time.Time) bool {
	if keyID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	w, ok := t.windows[keyID]
	if !ok {
		return false
	}
	if now.After(w.resetAt) {
		delete(t.windows, keyID)
		return false
	}
	return w.blocked
}

func (t *InjectionTracker) PenaltyRemaining(keyID string, now time.Time) time.Duration {
	if keyID == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	w, ok := t.windows[keyID]
	if !ok || now.After(w.resetAt) {
		return 0
	}
	if w.blocked {
		return w.resetAt.Sub(now)
	}
	return 0
}

func (t *InjectionTracker) Reset(keyID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.windows, keyID)
}

func (t *InjectionTracker) ActiveBlocks(now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, w := range t.windows {
		if w.blocked && now.Before(w.resetAt) {
			count++
		}
	}
	return count
}
