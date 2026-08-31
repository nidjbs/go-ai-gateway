package events

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// spyMetrics counts webhook delivery outcomes.
type spyMetrics struct {
	mu                sync.Mutex
	delivered, failed int
	dropped           int
}

func (s *spyMetrics) EventEmitted(context.Context, string) {}

func (s *spyMetrics) EventWebhookDelivered(context.Context, string) {
	s.mu.Lock()
	s.delivered++
	s.mu.Unlock()
}

func (s *spyMetrics) EventWebhookFailed(context.Context, string) {
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
}

func (s *spyMetrics) EventWebhookDropped(context.Context, string) {
	s.mu.Lock()
	s.dropped++
	s.mu.Unlock()
}

func (s *spyMetrics) counts() (delivered, failed, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delivered, s.failed, s.dropped
}

func TestWebhookDeliversEvent(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer recv.Close()

	hub, err := NewHub([]WebhookConfig{{Name: "t", URL: recv.URL, Retries: 0, Timeout: time.Second}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	hub.Emit(context.Background(), Event{Type: TypeRequestCompleted, Endpoint: "chat.completions", Alias: "chat"})

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if got[0].Type != TypeRequestCompleted || got[0].Endpoint != "chat.completions" || got[0].EventID == "" {
		t.Fatalf("unexpected event: %+v", got[0])
	}
}

func TestWebhookFiltersByType(t *testing.T) {
	var count atomic.Int64
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer recv.Close()

	hub, err := NewHub([]WebhookConfig{{Name: "t", URL: recv.URL, Events: []EventType{TypeRequestCompleted}, Retries: 0, Timeout: time.Second}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	hub.Emit(context.Background(), Event{Type: TypeRequestStarted})
	hub.Emit(context.Background(), Event{Type: TypeRequestCompleted})

	waitFor(t, func() bool { return count.Load() >= 1 })
	if count.Load() != 1 {
		t.Fatalf("delivered %d, want 1", count.Load())
	}
}

func TestWebhookRetriesThenFails(t *testing.T) {
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer recv.Close()
	m := &spyMetrics{}
	hub, err := NewHub([]WebhookConfig{{Name: "t", URL: recv.URL, Retries: 1, Timeout: time.Second}}, nil, m)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	hub.Emit(context.Background(), Event{Type: TypeRequestStarted})

	waitFor(t, func() bool {
		_, failed, _ := m.counts()
		return failed >= 1
	})
	delivered, failed, _ := m.counts()
	if delivered != 0 {
		t.Fatalf("delivered=%d, want 0", delivered)
	}
	if failed < 1 {
		t.Fatalf("failed=%d, want >= 1", failed)
	}
}

func TestWebhookQueueFullDrops(t *testing.T) {
	release := make(chan struct{})
	var count atomic.Int64
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer recv.Close()
	m := &spyMetrics{}
	hub, err := NewHub([]WebhookConfig{{Name: "t", URL: recv.URL, Queue: 1, Retries: 0, Timeout: time.Second}}, nil, m)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	// Overflow the single-slot queue while the worker is stuck on the receiver.
	for i := 0; i < 10; i++ {
		hub.Emit(context.Background(), Event{Type: TypeRequestStarted})
	}
	close(release)

	waitFor(t, func() bool {
		delivered, _, dropped := m.counts()
		return delivered > 0 && dropped > 0
	})
	delivered, _, dropped := m.counts()
	if delivered == 0 || dropped == 0 {
		t.Fatalf("delivered=%d dropped=%d, want both > 0", delivered, dropped)
	}
}
