package circuitbreaker

import "time"

// Noop is a Breaker implementation that never trips. Use when circuit
// breaking is disabled in configuration.
type Noop struct{}

func (Noop) Allow(string, time.Time) error   { return nil }
func (Noop) Record(string, time.Time, error) {}
