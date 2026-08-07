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

	stream, err := NewClient().OpenStream(context.Background(), json.RawMessage(`{"model":"test","stream":true}`), routing.Candidate{BaseURL: upstream.URL, Timeout: time.Second})
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
