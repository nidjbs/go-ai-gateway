package routing

import (
	"sync"
	"time"
)

// latencyAlpha is the EWMA smoothing factor for observed provider latencies.
// A smaller value weights history more; 0.2 keeps the estimate responsive to
// recent behavior while damping single-request outliers.
const latencyAlpha = 0.2

// LatencyTracker keeps an exponentially-weighted moving average of per-provider
// success latency, keyed by (provider name, model). It backs the
// least_latency routing strategy: each attempt records its observed duration
// and ordering reads the estimate back.
//
// The key space is bounded by the configured candidate set (one entry per
// provider+model), so the map never needs eviction.
type LatencyTracker struct {
	mu   sync.Mutex
	ewma map[string]time.Duration
}

// NewLatencyTracker returns an empty tracker.
func NewLatencyTracker() *LatencyTracker {
	return &LatencyTracker{ewma: make(map[string]time.Duration)}
}

// Record folds a successful attempt's duration into the candidate's EWMA.
// Durations are floored at zero; a zero recorded duration is stored so the
// candidate is treated as observed, not unknown.
func (t *LatencyTracker) Record(c Candidate, d time.Duration) {
	if d < 0 {
		d = 0
	}
	key := c.Name + "|" + c.Model
	t.mu.Lock()
	prev, ok := t.ewma[key]
	if !ok {
		t.ewma[key] = d
	} else {
		t.ewma[key] = time.Duration(latencyAlpha*float64(d) + (1-latencyAlpha)*float64(prev))
	}
	t.mu.Unlock()
}

// EWMA returns the candidate's estimated latency and whether it has been
// observed at least once.
func (t *LatencyTracker) EWMA(c Candidate) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d, ok := t.ewma[c.Name+"|"+c.Model]
	return d, ok
}
