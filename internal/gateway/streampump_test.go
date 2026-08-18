package gateway

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/light-llm-gateway/internal/provider"
)

// fakeStream is a minimal provider.Stream used to drive pumpStream in
// isolation. It serves queued events in order, then blocks Next() until Close
// is invoked (mimicking the upstream closing the response body). It counts
// Close invocations so tests can assert the closeOnce semantics.
type fakeStream struct {
	events    []provider.StreamEvent
	next      int
	done      chan struct{}
	closeHits atomic.Int32
	closed    atomic.Bool
	mu        sync.Mutex // protects next
}

func newFakeStream(events ...provider.StreamEvent) *fakeStream {
	return &fakeStream{events: events, done: make(chan struct{})}
}

func (s *fakeStream) Next() (provider.StreamEvent, error) {
	s.mu.Lock()
	if s.next < len(s.events) {
		e := s.events[s.next]
		s.next++
		s.mu.Unlock()
		return e, nil
	}
	s.mu.Unlock()
	<-s.done
	return provider.StreamEvent{}, io.EOF
}

func (s *fakeStream) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		s.closeHits.Add(1)
		close(s.done)
	}
	return nil
}

func TestPumpStreamForwardsEventsUntilClose(t *testing.T) {
	s := newFakeStream(
		provider.StreamEvent{Data: []byte(`{"a":1}`)},
		provider.StreamEvent{Data: []byte(`{"a":2}`)},
	)
	// Pre-close the fake so Next() unblocks with EOF after the two queued
	// events are drained. This lets the test function return without waiting
	// for the reader, while still exercising the EOF path.
	s.Close()

	out := pumpStream(context.Background(), s)

	var got [][]byte
	for r := range out {
		if r.err != nil {
			if r.err == io.EOF {
				continue
			}
			t.Fatalf("unexpected error: %v", r.err)
		}
		got = append(got, r.event.Data)
	}
	if len(got) != 2 || string(got[0]) != `{"a":1}` || string(got[1]) != `{"a":2}` {
		t.Fatalf("events = %q, want [{a:1} {a:2}]", got)
	}
	if hits := s.closeHits.Load(); hits != 1 {
		t.Fatalf("Close hit count = %d, want 1", hits)
	}
}

func TestPumpStreamClosesStreamOnContextCancel(t *testing.T) {
	// No events queued — Next() blocks on s.done. We cancel from the test side
	// and assert Close is invoked (watcher fired) and the channel closes
	// (reader returned).
	s := newFakeStream()
	ctx, cancel := context.WithCancel(context.Background())
	out := pumpStream(ctx, s)

	done := make(chan struct{})
	go func() {
		for range out {
		}
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpStream did not close after context cancel")
	}
	if s.closeHits.Load() == 0 {
		t.Fatal("Close was not invoked when ctx was cancelled")
	}
}
