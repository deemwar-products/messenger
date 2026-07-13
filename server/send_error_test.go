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

// The /send-502 asymmetry, root-caused and made loud: a webhook channel with no
// callbackURL is inbound-only, so an outbound /send can never succeed — that is a config
// precondition, not a gateway fault. It must answer 422 with an actionable reason (so a
// caller fixes config), NOT 502 (which reads as a transient upstream fault and invites
// endless retries). A channel WITH a callbackURL whose downstream genuinely fails still
// answers 502. The signed inbound path needs no callback and is unaffected.
func TestSend_InboundOnlyIs422_DownstreamFailureIs502(t *testing.T) {
	t.Setenv("HOOK_SECRET", "s")

	// A downstream that always fails (simulates a real gateway failure).
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer down.Close()

	cfg := &config.Config{Transports: map[string]config.Transport{
		// inbound-only: NO callbackURL — this is the channel that used to 502 silently.
		"hook": {Kind: "webhook", Enabled: true, TokenEnv: "HOOK_SECRET"},
		// outbound with a downstream that 500s.
		"broken": {Kind: "webhook", Enabled: true, TokenEnv: "HOOK_SECRET", Options: map[string]string{"callbackURL": down.URL}},
	}}
	box, err := inbox.Open(t.TempDir() + "/inbox.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	rt := channel.NewRuntime(cfg.Enabled(), channel.NewSecretResolver(nil), func(envelope.Envelope) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Up(ctx); err != nil {
		t.Fatal(err)
	}
	defer rt.Down()

	srv := httptest.NewServer(New(rt, box, "").Handler())
	defer srv.Close()

	// (1) /send to the inbound-only channel → 422 with a structured, actionable body.
	resp, err := http.Post(srv.URL+"/send", "application/json", strings.NewReader(`{"channel":"hook","text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("inbound-only send: want 422, got %d", resp.StatusCode)
	}
	var body struct {
		OK      bool   `json:"ok"`
		Reason  string `json:"reason"`
		Channel string `json:"channel"`
		Error   string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.OK || body.Reason != "inbound_only" || body.Channel != "hook" || body.Error == "" {
		t.Fatalf("422 body should be actionable: %+v", body)
	}

	// (2) /send to a channel whose downstream genuinely 500s → 502 (real gateway failure).
	r2, err := http.Post(srv.URL+"/send", "application/json", strings.NewReader(`{"channel":"broken","text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r2.StatusCode != http.StatusBadGateway {
		t.Fatalf("downstream failure: want 502, got %d", r2.StatusCode)
	}

	// (3) /health reports the configured channels; no streaming channels here, so streams
	// is present and empty (the field a monitor polls for liveness).
	h, _ := http.Get(srv.URL + "/health")
	var health struct {
		OK       bool                       `json:"ok"`
		Channels map[string]string          `json:"channels"`
		Streams  map[string]json.RawMessage `json:"streams"`
	}
	_ = json.NewDecoder(h.Body).Decode(&health)
	if !health.OK || health.Channels["hook"] != "webhook" || health.Streams == nil {
		t.Fatalf("health should list channels and a streams map: %+v", health)
	}
}
