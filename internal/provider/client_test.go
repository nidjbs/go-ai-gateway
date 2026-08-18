package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"example.com/light-llm-gateway/internal/routing"
)

func TestOpenStreamKeepsContextUntilClose(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()

	stream, err := NewClient().OpenStream(context.Background(), Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)}, routing.Candidate{BaseURL: upstream.URL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not start")
	}
	select {
	case <-canceled:
		t.Fatal("upstream context canceled before stream close")
	default:
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream context was not canceled on stream close")
	}
}

func TestProviderPoolIsolation(t *testing.T) {
	// Verify that each provider type gets its own *http.Client (connection pool)
	// and that calls to one provider do not affect the other's Transport.
	c := NewClient()

	types := c.RegisteredTypes()
	if len(types) == 0 {
		t.Fatal("expected at least one registered provider type")
	}

	// Every registered type must have a non-nil client.
	for _, typ := range types {
		cl := c.HTTPClient(typ)
		if cl == nil {
			t.Errorf("HTTPClient(%q) returned nil", typ)
			continue
		}
		if cl.Transport == nil {
			t.Errorf("HTTPClient(%q).Transport is nil — falls back to DefaultClient?", typ)
		}
	}

	// Two different types must not share the same *http.Client instance.
	seen := make(map[*http.Client]string)
	for _, typ := range types {
		cl := c.HTTPClient(typ)
		if prev, ok := seen[cl]; ok {
			t.Errorf("provider %q and %q share the same *http.Client instance — pools are not isolated", prev, typ)
		}
		seen[cl] = typ
	}

	// Unknown type falls back to "openai" client.
	fallback := c.HTTPClient("unknown-provider")
	openai := c.HTTPClient("openai")
	if fallback != openai {
		t.Error("unknown provider type did not fall back to the openai client")
	}
}

func TestSetHTTPClientReplacesPool(t *testing.T) {
	c := NewClient()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Swap in a custom client that points at the test server for "openai".
	custom := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(nil),
	}}
	c.SetHTTPClient("openai", custom)

	got := c.HTTPClient("openai")
	if got != custom {
		t.Error("SetHTTPClient did not replace the openai pool")
	}
}
