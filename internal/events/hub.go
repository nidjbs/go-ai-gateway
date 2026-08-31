package events

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Hub routes emitted events to in-process subscribers and webhook dispatchers.
// Emit never blocks: subscribers run in isolated goroutines and webhooks enqueue
// to bounded queues.
type Hub struct {
	log      *slog.Logger
	metrics  Metrics
	mu       sync.RWMutex
	subs     map[EventType][]Subscriber // per-type subscribers
	all      []Subscriber               // wildcard subscribers (empty types)
	webhooks []*webhookDispatcher
}

// NewHub builds a hub from webhook configs. metrics may be nil (no-op).
func NewHub(cfgs []WebhookConfig, log *slog.Logger, metrics Metrics) (*Hub, error) {
	if log == nil {
		log = slog.Default()
	}
	if metrics == nil {
		metrics = noopMetrics{}
	}
	h := &Hub{log: log, metrics: metrics, subs: make(map[EventType][]Subscriber)}
	for _, c := range cfgs {
		w, err := newWebhook(c, log, metrics)
		if err != nil {
			h.Close()
			return nil, err
		}
		h.webhooks = append(h.webhooks, w)
	}
	return h, nil
}

// Subscribe registers an in-process subscriber for the given event types; an
// empty types list subscribes to every type.
func (h *Hub) Subscribe(types []EventType, s Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(types) == 0 {
		h.all = append(h.all, s)
		return
	}
	for _, t := range types {
		h.subs[t] = append(h.subs[t], s)
	}
}

// Emit routes one event to matching subscribers and webhooks. It returns
// immediately; delivery is asynchronous and non-blocking.
func (h *Hub) Emit(ctx context.Context, ev Event) {
	if ev.EventID == "" {
		ev.EventID = NewEventID()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	h.mu.RLock()
	subs := append([]Subscriber(nil), h.subs[ev.Type]...)
	all := append([]Subscriber(nil), h.all...)
	webhooks := h.webhooks
	h.mu.RUnlock()

	h.metrics.EventEmitted(ctx, string(ev.Type))
	for _, s := range subs {
		h.runSubscriber(s, ev)
	}
	for _, s := range all {
		h.runSubscriber(s, ev)
	}
	for _, w := range webhooks {
		if w.matches(ev.Type) {
			w.enqueue(ev)
		}
	}
}

func (h *Hub) runSubscriber(s Subscriber, ev Event) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.log.Warn("event subscriber panicked", "subscriber", s.Name(), "type", ev.Type, "panic", r)
			}
		}()
		s.OnEvent(ev)
	}()
}

// Close stops all webhook workers.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.webhooks {
		w.Close()
	}
}

type noopMetrics struct{}

func (noopMetrics) EventEmitted(context.Context, string)          {}
func (noopMetrics) EventWebhookDelivered(context.Context, string) {}
func (noopMetrics) EventWebhookFailed(context.Context, string)    {}
func (noopMetrics) EventWebhookDropped(context.Context, string)   {}
