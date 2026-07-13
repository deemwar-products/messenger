package channel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deemwar-products/messenger/envelope"
)

// streamerFunc adapts a func to the Streamer interface for tests.
type streamerFunc func(context.Context, Publisher) error

func (f streamerFunc) Run(ctx context.Context, p Publisher) error { return f(ctx, p) }

// The supervisor records observable liveness for a streaming channel: the first Run fails,
// and /health (via StreamHealth) must reflect that it exited with the error, then that it
// restarted and is running again — the signal a monitor needs to catch a listener that
// died silently, with no host-process forensics.
func TestSupervise_TracksStreamLiveness(t *testing.T) {
	rt := NewRuntime(nil, nil, func(envelope.Envelope) {})
	rt.restartMin = time.Millisecond
	rt.restartMax = time.Millisecond

	runs := make(chan struct{}, 4)
	var mu sync.Mutex
	n := 0
	st := streamerFunc(func(ctx context.Context, _ Publisher) error {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		runs <- struct{}{}
		if cur == 1 {
			return errors.New("boom: listener died")
		}
		<-ctx.Done() // second run stays healthy until cancellation
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	rt.wg.Add(1)
	go rt.supervise(ctx, "fake", st)

	<-runs // first run started
	<-runs // second run started => the supervisor restarted after the failure

	got := rt.StreamHealth()["fake"]
	if !got.Running {
		t.Fatalf("after restart the stream should read running: %+v", got)
	}
	if got.Restarts < 1 {
		t.Fatalf("restart should be counted: %+v", got)
	}
	if !strings.Contains(got.LastErr, "boom") {
		t.Fatalf("the exit error should be recorded for observability: %+v", got)
	}
	if got.StartedAt.IsZero() {
		t.Fatalf("started_at should be set: %+v", got)
	}

	cancel()
	rt.wg.Wait()

	// After a clean, cancelled stop the last snapshot shows not-running.
	if rt.StreamHealth()["fake"].Running {
		t.Fatalf("after cancel the stream should read not running")
	}
}
