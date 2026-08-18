package gateway

import (
	"context"
	"sync"

	"example.com/light-llm-gateway/internal/provider"
)

// streamBufferSize bounds the in-flight queue between upstream reader and client
// writer; a full queue blocks the reader inside stream.Next() for backpressure.
const streamBufferSize = 16

// pumpResult carries one StreamEvent (or terminal error) to the writer goroutine.
type pumpResult struct {
	event provider.StreamEvent
	err   error
}

// pumpStream reads events from stream in a background goroutine and forwards
// them over a bounded channel. If ctx is canceled while the reader is blocked
// in stream.Next(), a side goroutine calls stream.Close() to unblock it.
func pumpStream(ctx context.Context, stream provider.Stream) <-chan pumpResult {
	out := make(chan pumpResult, streamBufferSize)
	stopWatcher := make(chan struct{})
	var closeOnce sync.Once
	closeStream := func() { closeOnce.Do(func() { _ = stream.Close() }) }

	go func() {
		select {
		case <-ctx.Done():
			closeStream()
		case <-stopWatcher:
		}
	}()

	go func() {
		defer close(out)
		defer close(stopWatcher)
		defer closeStream()
		for {
			event, err := stream.Next()
			select {
			case <-ctx.Done():
				return
			case out <- pumpResult{event: event, err: err}:
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}
