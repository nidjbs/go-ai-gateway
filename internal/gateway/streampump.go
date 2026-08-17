package gateway

import (
	"context"
	"sync"

	"example.com/light-llm-gateway/internal/provider"
)

// streamBufferSize bounds the in-flight queue between the upstream reader
// goroutine and the client-writer goroutine in chatStream. When the client
// falls behind, the queue fills and the reader goroutine blocks in
// stream.Next() (which is itself blocked on TCP reads), giving the upstream
// socket real OS-level backpressure.
//
// Sized for a couple of SSE batches; larger values trade latency for memory.
const streamBufferSize = 16

// pumpResult carries one StreamEvent (or terminal error) from the upstream
// reader goroutine to the writer goroutine.
type pumpResult struct {
	event provider.StreamEvent
	err   error
}

// pumpStream runs a background goroutine that reads events from stream and
// forwards them over a bounded channel. The caller drains the channel and
// writes to the HTTP response.
//
// Cancellation handling:
//
//   - If ctx is canceled (client disconnected, server shutting down) while the
//     reader is blocked inside stream.Next(), a side goroutine calls
//     stream.Close() so the blocked Read unblocks with an error and the
//     reader exits cleanly.
//   - The reader goroutine also checks ctx.Done() between events and closes
//     the stream itself when ctx is canceled.
//
// The two paths are intentionally redundant: whichever fires first wins,
// and the double-close is harmless because stream.Close() is idempotent in
// practice (cancel is set to nil after the first call).
func pumpStream(ctx context.Context, stream provider.Stream) <-chan pumpResult {
	out := make(chan pumpResult, streamBufferSize)
	stopWatcher := make(chan struct{})
	var closeOnce sync.Once
	closeStream := func() { closeOnce.Do(func() { _ = stream.Close() }) }

	// Watcher: as soon as ctx is canceled, force the stream closed so any
	// blocked Read on the upstream body returns. Runs concurrently with the
	// reader goroutine below.
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