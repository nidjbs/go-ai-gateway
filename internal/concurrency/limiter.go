// Package concurrency provides a counting semaphore for capping in-flight requests.
// A Limiter constructed with max=0 (or nil) is unlimited: TryAcquire always succeeds and Release is a no-op.
package concurrency

import (
	"context"
)

// Limiter caps concurrent holders. A nil *Limiter behaves as unlimited.
type Limiter struct {
	max int
	sem chan struct{}
}

// New returns a Limiter that allows up to max concurrent holders; max=0 means unlimited.
func New(max int) *Limiter {
	if max <= 0 {
		return &Limiter{}
	}
	return &Limiter{max: max, sem: make(chan struct{}, max)}
}

// TryAcquire reserves one slot if available. Returns false when the limit is reached.
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

// Release returns a previously acquired slot. Safe to call without a matching TryAcquire.
func (l *Limiter) Release() {
	if l == nil || l.max <= 0 {
		return
	}
	select {
	case <-l.sem:
	default:
		// Programmer error: releasing without a held slot; swallowed to keep defer safe.
	}
}

// Wait blocks until a slot is available or ctx is canceled. Returns true if a slot was acquired.
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
