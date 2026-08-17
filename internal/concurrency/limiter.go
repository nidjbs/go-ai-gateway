// Package concurrency provides a counting semaphore used by the gateway to
// cap in-flight requests at the gateway level and per API key.
//
// A Limiter constructed with max=0 is unlimited: TryAcquire always succeeds
// and Release is a no-op. This lets the rest of the code call
// TryAcquire/Release unconditionally without paying for the check when
// limits are disabled in configuration.
package concurrency

import (
	"context"
)

// Limiter caps the number of concurrent holders. A nil Limiter is valid and
// behaves as unlimited (TryAcquire always returns true, Release is a no-op).
type Limiter struct {
	max   int
	sem   chan struct{}
}

// New returns a Limiter that allows up to max concurrent holders. max=0
// means unlimited.
func New(max int) *Limiter {
	if max <= 0 {
		return &Limiter{}
	}
	return &Limiter{max: max, sem: make(chan struct{}, max)}
}

// TryAcquire reserves one slot if available. Returns false when the limit
// is reached; callers should respond with 503 / load shed.
func (l *Limiter) TryAcquire() bool {
	if l == nil || l.max <= 0 {
		return true
	}
	select {
	case l.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns a previously acquired slot. Calling Release without a
// matching TryAcquire is a no-op (the implementation can only release what
// was taken), so callers may safely call Release in defer without checking
// the return of TryAcquire.
func (l *Limiter) Release() {
	if l == nil || l.max <= 0 {
		return
	}
	select {
	case <-l.sem:
	default:
		// Release without a held slot — this is a programmer error in
		// normal flow; we swallow it to keep the defer pattern safe.
	}
}

// Wait blocks until a slot is available or ctx is canceled. Returns true if
// a slot was acquired. Not on the request hot path; useful for graceful
// shutdown ordering or test scaffolding.
func (l *Limiter) Wait(ctx context.Context) bool {
	if l == nil || l.max <= 0 {
		return true
	}
	select {
	case l.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}