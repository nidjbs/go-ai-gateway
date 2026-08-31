package events

import (
	"context"
	"sync"
	"testing"
	"time"
)

// collectSub records emitted events for assertions.
type collectSub struct {
	name  string
	panic bool
	mu    sync.Mutex
	got   []Event
}

func (s *collectSub) Name() string { return s.name }

func (s *collectSub) OnEvent(ev Event) {
	if s.panic {
		panic("boom")
	}
	s.mu.Lock()
	s.got = append(s.got, ev)
	s.mu.Unlock()
}

func (s *collectSub) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func (s *collectSub) types() []EventType {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EventType, len(s.got))
	for i, e := range s.got {
		out[i] = e.Type
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

func TestHubRoutesByType(t *testing.T) {
	hub, err := NewHub(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	sub := &collectSub{name: "typed"}
	hub.Subscribe([]EventType{TypeRequestCompleted}, sub)

	hub.Emit(context.Background(), Event{Type: TypeRequestStarted})
	hub.Emit(context.Background(), Event{Type: TypeRequestCompleted})

	waitFor(t, func() bool { return sub.len() == 1 })
	if types := sub.types(); len(types) != 1 || types[0] != TypeRequestCompleted {
		t.Fatalf("subscriber got %v", types)
	}
}

func TestHubWildcardAndPanicIsolation(t *testing.T) {
	hub, err := NewHub(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	panicky := &collectSub{name: "panicky", panic: true}
	normal := &collectSub{name: "normal"}
	hub.Subscribe(nil, panicky)
	hub.Subscribe([]EventType{TypeRequestStarted, TypeRequestCompleted}, normal)

	hub.Emit(context.Background(), Event{Type: TypeRequestStarted})
	hub.Emit(context.Background(), Event{Type: TypeRequestCompleted})

	// The panicking wildcard subscriber must not prevent the normal subscriber.
	waitFor(t, func() bool { return normal.len() == 2 })
}

func TestHubAssignsIDsAndTimestamps(t *testing.T) {
	hub, err := NewHub(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	sub := &collectSub{name: "id"}
	hub.Subscribe([]EventType{TypeRequestStarted}, sub)

	hub.Emit(context.Background(), Event{Type: TypeRequestStarted})

	waitFor(t, func() bool { return sub.len() == 1 })
	ev := sub.got[0]
	if ev.EventID == "" || ev.OccurredAt.IsZero() {
		t.Fatalf("event missing id/timestamp: %+v", ev)
	}
}

func TestNewHubRejectsUnknownType(t *testing.T) {
	_, err := NewHub([]WebhookConfig{{Name: "t", URL: "http://127.0.0.1:1", Events: []EventType{"bogus.type"}}}, nil, nil)
	if err == nil {
		t.Fatal("expected unknown type error")
	}
}
