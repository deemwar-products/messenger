package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/inbox"
)

// A subscription drains from its durable cursor in order, filters by channel, advances
// only on 2xx (a failing consumer retries the SAME message), and catches up after
// coming back.
func TestDispatcher_DurableCursorRetriesAndCatchesUp(t *testing.T) {
	dir := t.TempDir()
	box, err := inbox.Open(dir + "/inbox.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	_ = box.Append(envelope.Normalize(envelope.Envelope{ID: "m-1", Channel: "ops", Text: "one"}))
	_ = box.Append(envelope.Normalize(envelope.Envelope{ID: "m-2", Channel: "other", Text: "skip me"}))
	_ = box.Append(envelope.Normalize(envelope.Envelope{ID: "m-3", Channel: "ops", Text: "two"}))

	var failFirst atomic.Bool
	failFirst.Store(true)
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failFirst.Swap(false) {
			w.WriteHeader(http.StatusInternalServerError) // first delivery attempt fails
			return
		}
		var env envelope.Envelope
		_ = decode(r, &env)
		got = append(got, env.ID)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(box, dir+"/cursors", map[string]config.Subscription{
		"factory": {Enabled: true, URL: srv.URL, Channels: []string{"ops"}},
	})
	d.backoffMin = 10 * time.Millisecond
	d.tick = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go d.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if d.readCursor("factory") == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	if d.readCursor("factory") != 3 {
		t.Fatalf("cursor should reach 3, got %d", d.readCursor("factory"))
	}
	if len(got) != 2 || got[0] != "m-1" || got[1] != "m-3" {
		t.Fatalf("want [m-1 m-3] delivered in order (retry after fail, filtered other), got %v", got)
	}

	// New messages after a restart are delivered from the persisted cursor.
	_ = box.Append(envelope.Normalize(envelope.Envelope{ID: "m-4", Channel: "ops", Text: "three"}))
	d2 := New(box, dir+"/cursors", map[string]config.Subscription{
		"factory": {Enabled: true, URL: srv.URL, Channels: []string{"ops"}},
	})
	if n, err := d2.drain(context.Background(), "factory", d2.subs["factory"]); err != nil || n != 1 {
		t.Fatalf("catch-up drain: n=%d err=%v", n, err)
	}
	if got[len(got)-1] != "m-4" {
		t.Fatalf("want m-4 delivered on catch-up, got %v", got)
	}
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
