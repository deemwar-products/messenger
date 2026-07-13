package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/deemwar-products/messenger/channel"
	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/inbox"
	"github.com/deemwar-products/messenger/server"
	"github.com/deemwar-products/messenger/subscription"
)

// TestComplementarity_WorkerEventReachesCEOConsumer proves the organ↔organ interface: a
// worker/desk emits a status event (the same fields company-os `company log` records —
// agent, action, product, title — routed to a lane via thread_id) through messenger's
// signed inbound seam, and a CEO-side consumer subscription picks it up durably, in order,
// HMAC-verified. This is how the CEO "hears" the owner + workers + desks through this
// organ. NOTE (honest channel map): company-os itself logs to its SQLite ledger, not to
// messenger; messenger is the message transport. A company-log→messenger bridge is a named
// follow-up, not built here — this test asserts only what messenger genuinely delivers.
func TestComplementarity_WorkerEventReachesCEOConsumer(t *testing.T) {
	t.Setenv("MESSENGER_HOOK_SECRET", "hook-secret")
	t.Setenv("CEO_SECRET", "ceo-secret")

	var mu sync.Mutex
	var got envelope.Envelope
	var signed bool
	ceo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		signed = channel.VerifyHMAC([]byte("ceo-secret"), body, r.Header.Get("X-Messenger-Signature-256"))
		mu.Lock()
		_ = json.Unmarshal(body, &got)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ceo.Close()

	cfg := &config.Config{
		HookSecretEnv: "MESSENGER_HOOK_SECRET",
		Transports: map[string]config.Transport{
			// The organs lane: a signed inbound webhook workers/desks inject events on.
			"organs": {Kind: "webhook", Enabled: true, TokenEnv: "MESSENGER_HOOK_SECRET"},
		},
		Subscriptions: map[string]config.Subscription{
			// The CEO's ears: a durable consumer filtered to the organs lane.
			"ceo": {Enabled: true, URL: ceo.URL, Channels: []string{"organs"}, SecretEnv: "CEO_SECRET"},
		},
	}

	box, err := inbox.Open(t.TempDir() + "/inbox.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	disp := subscription.New(box, t.TempDir()+"/cursors", cfg.Subscriptions)
	rt := channel.NewRuntime(cfg.Enabled(), channel.NewSecretResolver(nil), fanout(box, disp, ""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Up(ctx); err != nil {
		t.Fatal(err)
	}
	defer rt.Down()
	go disp.Run(ctx)

	srv := server.New(rt, box, "")
	srv.UseHookSecret(cfg.HookSecretEnv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	defer hs.Close()
	base := "http://" + ln.Addr().String()
	waitHealthy(t, base)

	// A worker emits a company-log-shaped status event, routed to the organs lane.
	event := `{"sender":"organ-messenger","text":"harden: send+receive proven, /send 502 fixed","thread_id":"organs"}`
	if code := signedPost(t, base+"/webhook/organs", "hook-secret", event); code != http.StatusAccepted {
		t.Fatalf("worker inject: want 202, got %d", code)
	}

	waitUntil(t, "CEO consumer receives the worker event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got.Sender == "organ-messenger" && strings.Contains(got.Text, "harden") && got.ThreadID == "organs"
	})
	if !signed {
		t.Fatalf("delivery to the CEO consumer was not HMAC-signed")
	}
}
