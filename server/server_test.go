package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deemwar-products/messenger/channel"
	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/inbox"
)

// serve round-trip: POST /send delivers via the webhook channel to a captured callback
// with the reply threaded and returns an id; GET /inbox?since=N returns appended
// messages; bearer auth is enforced.
func TestServe_SendThreadsAndInboxReadsSince(t *testing.T) {
	t.Setenv("HOOK_SECRET", "s")
	// A downstream that captures what messenger delivers on the "out" channel.
	var delivered map[string]any
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&delivered)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer cb.Close()

	cfg := &config.Config{Transports: map[string]config.Transport{
		"out": {Kind: "webhook", Enabled: true, TokenEnv: "HOOK_SECRET", Options: map[string]string{"callbackURL": cb.URL}},
	}}
	box, err := inbox.Open(t.TempDir() + "/inbox.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	_ = box.Append(mkEnv("m-1", "first"))
	_ = box.Append(mkEnv("m-2", "second"))

	rt := channel.NewRuntime(cfg.Enabled(), channel.NewSecretResolver(nil), func(envelope.Envelope) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Up(ctx); err != nil {
		t.Fatal(err)
	}
	defer rt.Down()

	s := New(rt, box, "tok")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// POST /send without the bearer → 401.
	noauth, _ := http.Post(srv.URL+"/send", "application/json", strings.NewReader(`{"channel":"out","text":"x"}`))
	if noauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without bearer, got %d", noauth.StatusCode)
	}

	// POST /send with bearer + reply_to → delivered, threaded, id returned.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/send",
		strings.NewReader(`{"channel":"out","text":"reply body","to":"chat-1","reply_to":"m-2"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("send failed: err=%v code=%v", err, resp.StatusCode)
	}
	var sent struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sent)
	if !sent.OK || sent.ID == "" {
		t.Fatalf("send should return an id: %+v", sent)
	}
	if delivered["text"] != "reply body" || delivered["reply_to"] != "m-2" || delivered["thread_id"] != "chat-1" {
		t.Fatalf("bad delivered envelope: %v", delivered)
	}

	// POST /send with reply_to "last" → threads onto the NEWEST inbound message on the
	// channel and inherits its thread, no id bookkeeping.
	lr, _ := http.NewRequest(http.MethodPost, srv.URL+"/send",
		strings.NewReader(`{"channel":"out","text":"auto reply","reply_to":"last"}`))
	lr.Header.Set("Authorization", "Bearer tok")
	lresp, err := http.DefaultClient.Do(lr)
	if err != nil || lresp.StatusCode != http.StatusOK {
		t.Fatalf("reply-last failed: err=%v code=%v", err, lresp.StatusCode)
	}
	if delivered["reply_to"] != "m-2" || delivered["thread_id"] != "t-2" {
		t.Fatalf("reply last should target m-2 in t-2: %v", delivered)
	}

	// GET /inbox?since=1 → only the second message.
	ir, _ := http.NewRequest(http.MethodGet, srv.URL+"/inbox?since=1", nil)
	ir.Header.Set("Authorization", "Bearer tok")
	iresp, _ := http.DefaultClient.Do(ir)
	var out struct {
		Messages []map[string]any `json:"messages"`
		Next     int              `json:"next"`
	}
	_ = json.NewDecoder(iresp.Body).Decode(&out)
	if len(out.Messages) != 1 || out.Messages[0]["text"] != "second" || out.Next != 2 {
		t.Fatalf("bad inbox since: %+v", out)
	}
}

func mkEnv(id, text string) envelope.Envelope {
	return envelope.Normalize(envelope.Envelope{ID: id, Channel: "out", Text: text, ThreadID: "t-" + id[len(id)-1:]})
}

// signedHook computes the X-Hub-Signature-256 a peer sends and POSTs the body to url.
func signedHook(t *testing.T, url, secret, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", channel.SignHMAC([]byte(secret), []byte(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("hook post: %v", err)
	}
	return resp
}

// The universal hook is symmetric: a signed peer pushes a message IN (/hook/send) and it
// lands in the inbox with its attachment; a bad signature is 401; deliver:true routes the
// message OUT through a channel to the peer's callbackURL signed the same way; /hook/recv
// polls messages back OUT filtered by channel. One shared secret, HMAC both directions.
func TestUniversalHook_SendRecvAndAuth(t *testing.T) {
	const secret = "peer-shared-secret"
	t.Setenv("MESSENGER_HOOK_SECRET", secret)

	// A peer messenger's inbound endpoint: captures what we deliver OUT to it.
	var delivered map[string]any
	var deliveredSig string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveredSig = r.Header.Get("X-Hub-Signature-256")
		_ = json.NewDecoder(r.Body).Decode(&delivered)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer peer.Close()

	// The peer is a webhook channel with its callbackURL = the peer's inbound endpoint,
	// signed with the same shared secret — this is how another messenger is "registered".
	cfg := &config.Config{Transports: map[string]config.Transport{
		"peer": {Kind: "webhook", Enabled: true, TokenEnv: "MESSENGER_HOOK_SECRET", Options: map[string]string{"callbackURL": peer.URL}},
	}}
	box, err := inbox.Open(t.TempDir() + "/inbox.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	// Publisher appends inbound to the box, exactly as `serve`'s fanout does.
	rt := channel.NewRuntime(cfg.Enabled(), channel.NewSecretResolver(nil), func(e envelope.Envelope) { _ = box.Append(e) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Up(ctx); err != nil {
		t.Fatal(err)
	}
	defer rt.Down()

	s := New(rt, box, "tok") // bearer differs from the hook secret; hook uses HMAC only
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// (1) Push IN: signed → 202, envelope injected into the inbox with its attachment.
	in := `{"channel":"hook","id":"peer-1","text":"hello from peer","sender":"peerbot",` +
		`"attachments":[{"type":"image","name":"pic.png","mime":"image/png","url":"http://peer/media/pic.png"}]}`
	if resp := signedHook(t, srv.URL+"/hook/send", secret, in); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("hook send: want 202, got %d", resp.StatusCode)
	}
	msgs, _, err := box.Since(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hello from peer" || msgs[0].Channel != "hook" {
		t.Fatalf("inbound not injected: %+v", msgs)
	}
	if len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].URL != "http://peer/media/pic.png" || msgs[0].Attachments[0].Path != "" {
		t.Fatalf("attachment not carried (url-only): %+v", msgs[0].Attachments)
	}

	// (2) Bad signature → 401, nothing injected.
	bad, _ := http.NewRequest(http.MethodPost, srv.URL+"/hook/send", strings.NewReader(in))
	bad.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	if resp, _ := http.DefaultClient.Do(bad); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad sig: want 401, got %d", resp.StatusCode)
	}

	// (3) Deliver OUT: deliver:true routes through the "peer" channel to its callbackURL,
	// signed with the shared secret — the symmetric reverse of step (1).
	out := `{"channel":"peer","deliver":true,"text":"reply to peer","attachments":[{"type":"file","name":"a.txt","url":"http://self/media/a.txt"}]}`
	dr := signedHook(t, srv.URL+"/hook/send", secret, out)
	if dr.StatusCode != http.StatusOK {
		t.Fatalf("hook deliver: want 200, got %d", dr.StatusCode)
	}
	if delivered["text"] != "reply to peer" {
		t.Fatalf("peer did not receive delivery: %v", delivered)
	}
	if deliveredSig == "" {
		t.Fatalf("delivery to peer callbackURL was not HMAC-signed")
	}

	// (4) Poll OUT via /hook/recv, filtered to channel "hook" → only the inbound message.
	rr := signedHook(t, srv.URL+"/hook/recv", secret, `{"since":0,"channels":["hook"]}`)
	if rr.StatusCode != http.StatusOK {
		t.Fatalf("hook recv: want 200, got %d", rr.StatusCode)
	}
	var recv struct {
		OK       bool             `json:"ok"`
		Messages []map[string]any `json:"messages"`
		Next     int              `json:"next"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&recv)
	if !recv.OK || len(recv.Messages) != 1 || recv.Messages[0]["text"] != "hello from peer" {
		t.Fatalf("recv filtered wrong: %+v", recv)
	}

	// (5) /hook/recv with a bad signature → 401.
	badr, _ := http.NewRequest(http.MethodPost, srv.URL+"/hook/recv", strings.NewReader(`{"since":0}`))
	badr.Header.Set("X-Hub-Signature-256", "sha256=nope")
	if resp, _ := http.DefaultClient.Do(badr); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("recv bad sig: want 401, got %d", resp.StatusCode)
	}
}
