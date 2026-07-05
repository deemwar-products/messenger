package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deemwar-products/messenger/channel"
	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// inject round-trip: the body is signed exactly as the hub verifies it — the REAL
// webhook channel handler stands in for the hub — the envelope lands with the minted
// id, and a wrong secret surfaces the 401 hint.
func TestInject_SignsPostsAndHandles401(t *testing.T) {
	t.Setenv("HOOK_SECRET", "s3cr3t")
	cfg := config.Transport{Kind: "webhook", TokenEnv: "HOOK_SECRET"}
	k, err := channel.KindFor("in", cfg)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := k.Open("in", cfg, channel.NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	var got envelope.Envelope
	mux := http.NewServeMux()
	mux.Handle("/webhook/in", ch.(channel.Pushed).Handler(func(e envelope.Envelope) { got = e }))
	hub := httptest.NewServer(mux)
	defer hub.Close()

	body, id, err := injectBody("hello", "ci", "run-42", "m-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := injectPost(hub.Client(), hub.URL, "/webhook/in", "X-Hub-Signature-256", []byte("s3cr3t"), body); err != nil {
		t.Fatalf("signed inject should land: %v", err)
	}
	if got.Text != "hello" || got.Sender != "ci" || got.ThreadID != "run-42" || got.ReplyTo != "m-7" || got.ID != id {
		t.Fatalf("bad envelope: %+v (want id %s)", got, id)
	}

	// Wrong secret → the hub 401s and the error carries the mismatch hint.
	err = injectPost(hub.Client(), hub.URL, "/webhook/in", "X-Hub-Signature-256", []byte("wrong"), body)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 hint, got %v", err)
	}
}

// injectBody omits empty fields and the minted id rides in the signed bytes.
func TestInjectBody_OmitsEmpties(t *testing.T) {
	body, id, err := injectBody("hi", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if strings.Contains(s, "sender") || strings.Contains(s, "thread_id") || strings.Contains(s, "reply_to") {
		t.Fatalf("empties should be omitted: %s", s)
	}
	if id == "" || !strings.Contains(s, id) {
		t.Fatalf("minted id should ride in the body: id=%q body=%s", id, s)
	}
}

// injectPost against a dead hub returns the "start it" hint, never a secret.
func TestInjectPost_DeadHubHint(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	hub.Close()
	err := injectPost(http.DefaultClient, hub.URL, "/webhook/in", "X-Hub-Signature-256", []byte("s"), []byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "messenger serve") {
		t.Fatalf("want unreachable hint, got %v", err)
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("secret leaked into error: %v", err)
	}
}
