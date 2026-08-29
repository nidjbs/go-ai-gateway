package circuitbreaker

import (
	"errors"
	"testing"
	"time"
)

const testKey = "provider|http://upstream"

var t0 = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// rateCfg keeps the consecutive fast path inert (threshold 10) so error-rate
// tests exercise only the sliding window. OpenDuration is long enough that an
// open breaker never re-enters half-open mid-test.
func rateCfg() Config {
	return Config{
		FailureThreshold:         10,
		OpenDuration:             10 * time.Minute,
		HalfOpenMaxRequests:      1,
		HalfOpenSuccessThreshold: 1,
		ErrorRate:                0.5,
		Window:                   5 * time.Minute,
		MinSamples:               4,
	}
}

func record(b Breaker, at time.Time, fail bool) {
	var err error
	if fail {
		err = errors.New("boom")
	}
	b.Record(testKey, at, err)
}

// assertOpen checks the breaker's current state via Allow. In Closed state
// Allow never mutates the entry; in Open state it may transition to Half-Open
// once OpenDuration elapses, so callers must keep `at` close to trip time.
func assertOpen(t *testing.T, b Breaker, at time.Time, want bool) {
	t.Helper()
	if got := b.Allow(testKey, at) != nil; got != want {
		t.Fatalf("open=%v, want %v", got, want)
	}
}

func TestErrorRateTripsOpen(t *testing.T) {
	b := New(rateCfg())
	record(b, t0, true)
	record(b, t0.Add(10*time.Millisecond), false)
	record(b, t0.Add(20*time.Millisecond), true)
	assertOpen(t, b, t0.Add(20*time.Millisecond), false) // total 3 < min 4
	record(b, t0.Add(30*time.Millisecond), true)
	assertOpen(t, b, t0.Add(30*time.Millisecond), true) // 3/4 = 75% >= 50%
}

func TestErrorRateRespectsMinSamples(t *testing.T) {
	cfg := rateCfg()
	cfg.MinSamples = 3
	b := New(cfg)
	b2 := New(cfg) // separate breaker for the min-samples variant
	record(b, t0, true)
	record(b, t0.Add(10*time.Millisecond), false)
	assertOpen(t, b, t0.Add(10*time.Millisecond), false) // 50% but total 2 < 4
	record(b2, t0, true)
	record(b2, t0.Add(10*time.Millisecond), false)
	record(b2, t0.Add(20*time.Millisecond), true)
	assertOpen(t, b2, t0.Add(20*time.Millisecond), true) // 2/3 = 66.7% >= 50%
}

func TestErrorRateAtBoundaryTrips(t *testing.T) {
	b := New(rateCfg())
	record(b, t0, true)
	record(b, t0.Add(10*time.Millisecond), false)
	record(b, t0.Add(20*time.Millisecond), false)
	record(b, t0.Add(30*time.Millisecond), true)
	assertOpen(t, b, t0.Add(30*time.Millisecond), true) // exactly 2/4 = 50% trips (inclusive)
}

func TestErrorRateBelowThresholdStaysClosed(t *testing.T) {
	b := New(rateCfg())
	for i, fail := range []bool{true, false, false, false, true, false, false, false, false, false} {
		record(b, t0.Add(time.Duration(i)*10*time.Millisecond), fail)
	}
	assertOpen(t, b, t0.Add(100*time.Millisecond), false) // peak 2/5 = 40% < 50%
}

func TestConsecutiveTripStillWorks(t *testing.T) {
	cfg := rateCfg()
	cfg.FailureThreshold = 3
	cfg.MinSamples = 100 // rate path inert
	b := New(cfg)
	record(b, t0, true)
	record(b, t0.Add(10*time.Millisecond), true)
	assertOpen(t, b, t0.Add(10*time.Millisecond), false)
	record(b, t0.Add(20*time.Millisecond), true)
	assertOpen(t, b, t0.Add(20*time.Millisecond), true) // 3 consecutive via fast path
}

func TestConsecutiveResetBySuccess(t *testing.T) {
	cfg := rateCfg()
	cfg.FailureThreshold = 3
	cfg.MinSamples = 100
	b := New(cfg)
	record(b, t0, true)
	record(b, t0.Add(10*time.Millisecond), false)
	record(b, t0.Add(20*time.Millisecond), true)
	record(b, t0.Add(30*time.Millisecond), false)
	assertOpen(t, b, t0.Add(30*time.Millisecond), false)
	record(b, t0.Add(40*time.Millisecond), true)
	record(b, t0.Add(50*time.Millisecond), true)
	assertOpen(t, b, t0.Add(50*time.Millisecond), false) // 2 consecutive
	record(b, t0.Add(60*time.Millisecond), true)
	assertOpen(t, b, t0.Add(60*time.Millisecond), true) // 3rd consecutive
}

func TestSuccessCountsAsSampleButResetsStreak(t *testing.T) {
	cfg := rateCfg()
	cfg.MinSamples = 2
	cfg.FailureThreshold = 3
	b := New(cfg)
	record(b, t0, false)
	record(b, t0.Add(10*time.Millisecond), false)
	record(b, t0.Add(20*time.Millisecond), false)
	record(b, t0.Add(30*time.Millisecond), true)
	assertOpen(t, b, t0.Add(30*time.Millisecond), false) // 1/4 = 25%, streak 1
}

func TestWindowPruningAgesOutOldFailures(t *testing.T) {
	cfg := rateCfg()
	cfg.Window = 60 * time.Second
	cfg.MinSamples = 3
	cfg.FailureThreshold = 100
	b := New(cfg)
	record(b, t0, true)
	record(b, t0.Add(1*time.Second), false)
	record(b, t0.Add(2*time.Second), false)
	record(b, t0.Add(61*time.Second), true)
	assertOpen(t, b, t0.Add(61*time.Second), false) // F@0 pruned; 1/3 = 33%
	record(b, t0.Add(62*time.Second), true)
	assertOpen(t, b, t0.Add(62*time.Second), true) // S@1s pruned; 2/3 = 66.7%
}

func TestHalfOpenRecovery(t *testing.T) {
	cfg := rateCfg()
	cfg.FailureThreshold = 3
	cfg.OpenDuration = 10 * time.Millisecond
	cfg.MinSamples = 100
	b := New(cfg)
	record(b, t0, true)
	record(b, t0.Add(1*time.Millisecond), true)
	record(b, t0.Add(2*time.Millisecond), true)
	assertOpen(t, b, t0.Add(2*time.Millisecond), true)
	assertOpen(t, b, t0.Add(2*time.Millisecond), true)   // still open, no probe yet
	assertOpen(t, b, t0.Add(13*time.Millisecond), false) // -> half-open, probe allowed
	assertOpen(t, b, t0.Add(13*time.Millisecond), true)  // probe busy cap reached
	record(b, t0.Add(13*time.Millisecond), false)
	assertOpen(t, b, t0.Add(13*time.Millisecond), false) // probe success -> closed
}

func TestHalfOpenProbeFailureReopens(t *testing.T) {
	cfg := rateCfg()
	cfg.FailureThreshold = 3
	cfg.OpenDuration = 10 * time.Millisecond
	cfg.MinSamples = 100
	b := New(cfg)
	record(b, t0, true)
	record(b, t0.Add(1*time.Millisecond), true)
	record(b, t0.Add(2*time.Millisecond), true)
	assertOpen(t, b, t0.Add(13*time.Millisecond), false) // half-open probe allowed
	record(b, t0.Add(13*time.Millisecond), true)
	assertOpen(t, b, t0.Add(13*time.Millisecond), true)  // probe failure -> open again
	assertOpen(t, b, t0.Add(24*time.Millisecond), false) // 11ms later, half-open again
}

func TestOpenStateRecordsIgnored(t *testing.T) {
	cfg := rateCfg()
	cfg.FailureThreshold = 3
	cfg.OpenDuration = 10 * time.Millisecond
	cfg.MinSamples = 100
	b := New(cfg)
	record(b, t0, true)
	record(b, t0.Add(1*time.Millisecond), true)
	record(b, t0.Add(2*time.Millisecond), true)
	record(b, t0.Add(5*time.Millisecond), true)          // must be ignored: no state change, no openedAt reset
	assertOpen(t, b, t0.Add(5*time.Millisecond), true)   // still open
	assertOpen(t, b, t0.Add(12*time.Millisecond), false) // 10ms since original trip (2ms), not 5ms
}

func TestErrorRateExactlyOneRequiresAllFailures(t *testing.T) {
	cfg := rateCfg()
	cfg.ErrorRate = 1.0
	cfg.MinSamples = 3
	cfg.FailureThreshold = 10
	a := New(cfg)
	record(a, t0, true)
	record(a, t0.Add(10*time.Millisecond), false)
	record(a, t0.Add(20*time.Millisecond), true)
	assertOpen(t, a, t0.Add(20*time.Millisecond), false) // 2/3 < 100%
	b := New(cfg)
	record(b, t0, true)
	record(b, t0.Add(10*time.Millisecond), true)
	record(b, t0.Add(20*time.Millisecond), true)
	assertOpen(t, b, t0.Add(20*time.Millisecond), true) // 3/3 = 100%
}

func TestMinSamplesOneTripsOnFirstFailure(t *testing.T) {
	cfg := rateCfg()
	cfg.MinSamples = 1
	b := New(cfg)
	record(b, t0, true)
	assertOpen(t, b, t0, true) // 1/1 = 100% >= 50%
}

func TestNewClampsDefaults(t *testing.T) {
	mb := New(Config{}).(*memoryBreaker)
	want := Config{FailureThreshold: 10, OpenDuration: 30 * time.Second,
		HalfOpenMaxRequests: 1, HalfOpenSuccessThreshold: 1,
		ErrorRate: 0.5, Window: 60 * time.Second, MinSamples: 10}
	if mb.cfg != want {
		t.Fatalf("cfg = %+v, want %+v", mb.cfg, want)
	}
}

func TestNewClampsInvalidValues(t *testing.T) {
	mb := New(Config{FailureThreshold: 0, ErrorRate: 2, Window: 0, MinSamples: 0}).(*memoryBreaker)
	want := Config{FailureThreshold: 10, OpenDuration: 30 * time.Second,
		HalfOpenMaxRequests: 1, HalfOpenSuccessThreshold: 1,
		ErrorRate: 0.5, Window: 60 * time.Second, MinSamples: 10}
	if mb.cfg != want {
		t.Fatalf("cfg = %+v, want %+v", mb.cfg, want)
	}
}
