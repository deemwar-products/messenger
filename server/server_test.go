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
	return envelope.Normalize(envelope.Envelope{ID: id, Channel: "out", Text: text})
}
