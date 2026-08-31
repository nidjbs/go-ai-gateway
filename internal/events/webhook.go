package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for webhook delivery; applied when a config field is unset.
const (
	defaultWebhookQueue   = 4096
	defaultWebhookTimeout = 5 * time.Second
	defaultWebhookRetries = 2
)

// WebhookConfig is the runtime shape of one webhook delivery target. An empty
// Events list subscribes to every event type.
type WebhookConfig struct {
	Name    string
	URL     string
	Headers map[string]string
	Events  []EventType
	Queue   int
	Timeout time.Duration
	Retries int
}

func (c WebhookConfig) normalize() WebhookConfig {
	if c.Queue <= 0 {
		c.Queue = defaultWebhookQueue
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultWebhookTimeout
	}
	if c.Retries < 0 {
		c.Retries = defaultWebhookRetries
	}
	return c
}

// webhookDispatcher delivers events to one target over a bounded queue. When
// the queue is full events are dropped (fail-open) so delivery pressure never
// reaches the request path.
type webhookDispatcher struct {
	cfg       WebhookConfig
	types     map[EventType]bool // nil = all
	queue     chan Event
	client    *http.Client
	log       *slog.Logger
	metrics   Metrics
	done      chan struct{}
	closed    atomic.Bool
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func newWebhook(cfg WebhookConfig, log *slog.Logger, metrics Metrics) (*webhookDispatcher, error) {
	cfg = cfg.normalize()
	var types map[EventType]bool
	if len(cfg.Events) > 0 {
		types = make(map[EventType]bool, len(cfg.Events))
		for _, t := range cfg.Events {
			if !IsKnownType(t) {
				return nil, fmt.Errorf("events: webhook %q: unknown event type %q", cfg.Name, t)
			}
			types[t] = true
		}
	}
	d := &webhookDispatcher{
		cfg:     cfg,
		types:   types,
		queue:   make(chan Event, cfg.Queue),
		client:  &http.Client{},
		log:     log,
		metrics: metrics,
		done:    make(chan struct{}),
	}
	d.wg.Add(1)
	go d.loop()
	return d, nil
}

func (d *webhookDispatcher) matches(t EventType) bool {
	if d.types == nil {
		return true
	}
	return d.types[t]
}

// enqueue hands an event to the worker without blocking; a full queue drops it.
func (d *webhookDispatcher) enqueue(ev Event) bool {
	if d.closed.Load() {
		return false
	}
	select {
	case d.queue <- ev:
		return true
	default:
		d.metrics.EventWebhookDropped(context.Background(), d.cfg.Name)
		d.log.Warn("event webhook queue full, dropping", "webhook", d.cfg.Name, "type", ev.Type)
		return false
	}
}

func (d *webhookDispatcher) loop() {
	defer d.wg.Done()
	for {
		select {
		case ev := <-d.queue:
			d.deliver(ev)
		case <-d.done:
			return
		}
	}
}

func (d *webhookDispatcher) deliver(ev Event) {
	body, err := json.Marshal(ev)
	if err != nil {
		d.log.Warn("event webhook marshal failed", "webhook", d.cfg.Name, "error", err)
		return
	}
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
			case <-d.done:
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Timeout)
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.URL, bytes.NewReader(body))
		if rerr == nil {
			req.Header.Set("Content-Type", "application/json")
			for k, v := range d.cfg.Headers {
				req.Header.Set(k, v)
			}
			if resp, derr := d.client.Do(req); derr == nil {
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					cancel()
					d.metrics.EventWebhookDelivered(context.Background(), d.cfg.Name)
					return
				}
			}
		}
		cancel()
		if attempt >= d.cfg.Retries {
			break
		}
	}
	d.metrics.EventWebhookFailed(context.Background(), d.cfg.Name)
	d.log.Warn("event webhook delivery failed", "webhook", d.cfg.Name, "url", d.cfg.URL, "type", ev.Type)
}

// Close stops the worker, dropping any queued events.
func (d *webhookDispatcher) Close() {
	d.closeOnce.Do(func() {
		d.closed.Store(true)
		close(d.done)
		d.wg.Wait()
	})
}
