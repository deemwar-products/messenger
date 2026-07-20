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
	"time"

	"github.com/deemwar-products/messenger/channel"
	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/inbox"
	"github.com/deemwar-products/messenger/server"
	"github.com/deemwar-products/messenger/subscription"
)

// TestCEO_SendAndReceiveEndToEnd is the CEO use-case receipt: a REAL hub on a free port,
// wired exactly as `serve` wires it (the real fanout → inbox + subscription Dispatcher →
// consumer), proving the ears + mouth end to end WITHOUT touching the live :14310 hub.
//
//	(a) inbound: a signed webhook POST lands in the durable inbox AND is pushed, in order,
//	    to a durable consumer subscription — HMAC-signed with X-Messenger-Signature-256.
//	(b) outbound: POST /send delivers through a channel to its callback, threaded on the
//	    prior message, and returns the provider id.
//	(c) the /send-502 asymmetry, root-caused: an inbound-only channel answers a LOUD 422
//	    (reason inbound_only), while the signed webhook path works — so the failure is no
//	    longer a silent gateway error.
func TestCEO_SendAndReceiveEndToEnd(t *testing.T) {
	t.Setenv("MESSENGER_HOOK_SECRET", "hook-secret")
	t.Setenv("CONSUMER_SECRET", "consumer-secret")

	// A durable consumer: records every pushed envelope and asserts the HMAC signature.
	var mu sync.Mutex
	var received []envelope.Envelope
	var sigVerified bool
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sig := r.Header.Get("X-Messenger-Signature-256")
		var env envelope.Envelope
		_ = json.Unmarshal(body, &env)
		mu.Lock()
		received = append(received, env)
		sigVerified = channel.VerifyHMAC([]byte("consumer-secret"), body, sig)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer consumer.Close()

	// The outbound channel's downstream: captures what the mouth delivers.
	var delivered map[string]any
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&delivered)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer callback.Close()

	cfg := &config.Config{
		HookSecretEnv: "MESSENGER_HOOK_SECRET",
		Transports: map[string]config.Transport{
			"in":      {Kind: "webhook", Enabled: true, TokenEnv: "MESSENGER_HOOK_SECRET"},
			"out":     {Kind: "webhook", Enabled: true, TokenEnv: "MESSENGER_HOOK_SECRET", Options: map[string]string{"callbackURL": callback.URL}},
			"deadend": {Kind: "webhook", Enabled: true, TokenEnv: "MESSENGER_HOOK_SECRET"},
		},
		Subscriptions: map[string]config.Subscription{
			"consumer": {Enabled: true, URL: consumer.URL, SecretEnv: "CONSUMER_SECRET"},
		},
	}

	// Wire the hub with the REAL serve plumbing: fanout → inbox + dispatcher.
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

	// A real listener on a free port — not httptest — so this is a genuine hub socket.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	defer hs.Close()
	base := "http://" + ln.Addr().String()

	// Sanity: /health self-identifies the hub and carries the streams liveness field.
	waitHealthy(t, base)

	// (a) INBOUND: a signed webhook POST → durable inbox → consumer subscription push.
	inBody := `{"text":"owner: status?","sender":"owner","id":"in-1"}`
	if code := signedPost(t, base+"/webhook/in", "hook-secret", inBody); code != http.StatusAccepted {
		t.Fatalf("inbound webhook: want 202, got %d", code)
	}
	waitUntil(t, "consumer receives inbound", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1 && received[0].Text == "owner: status?"
	})
	mu.Lock()
	if !sigVerified {
		t.Fatalf("consumer push was not HMAC-signed / verified")
	}
	firstID := received[0].ID
	mu.Unlock()

	// It is also durably in the inbox (the queue behind the subscription).
	msgs, _, err := box.Since(0)
	if err != nil || len(msgs) != 1 || msgs[0].Channel != "in" {
		t.Fatalf("inbound not durable in inbox: %+v (err %v)", msgs, err)
	}

	// (b) OUTBOUND: POST /send through "out", threaded onto the inbound message.
	sendBody := `{"channel":"out","text":"ceo: all green","to":"ops","reply_to":"` + firstID + `"}`
	resp, err := http.Post(base+"/send", "application/json", strings.NewReader(sendBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("outbound /send failed: err=%v code=%v", err, resp.StatusCode)
	}
	var sent struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sent)
	if !sent.OK || sent.ID == "" {
		t.Fatalf("send should return a provider id: %+v", sent)
	}
	if delivered["text"] != "ceo: all green" || delivered["reply_to"] != firstID || delivered["thread_id"] != "ops" {
		t.Fatalf("outbound not delivered/threaded: %v", delivered)
	}

	// (c) THE ASYMMETRY, made loud: /send to an inbound-only channel → 422, not a silent 502.
	dead, err := http.Post(base+"/send", "application/json", strings.NewReader(`{"channel":"deadend","text":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if dead.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("inbound-only /send: want 422, got %d", dead.StatusCode)
	}
	var derr struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(dead.Body).Decode(&derr)
	if derr.Reason != "inbound_only" {
		t.Fatalf("422 should name the reason: %q", derr.Reason)
	}
}

// waitHealthy blocks until /health answers as the messenger hub.
func waitHealthy(t *testing.T, base string) {
	t.Helper()
	waitUntil(t, "hub healthy", func() bool {
		resp, err := http.Get(base + "/health")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var h struct {
			Service string          `json:"service"`
			Streams json.RawMessage `json:"streams"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&h)
		return h.Service == "messenger" && h.Streams != nil
	})
}

// signedPost signs body with the shared secret exactly as the hub verifies it and returns
// the status code.
func signedPost(t *testing.T, url, secret, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", channel.SignHMAC([]byte(secret), []byte(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("signed post: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// waitUntil polls cond for up to 3s, failing the test with what it was waiting for.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
