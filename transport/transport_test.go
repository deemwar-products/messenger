package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// hook inbound: a correctly-signed POST normalizes to an Envelope carrying thread + reply;
// a bad signature is rejected 401.
func TestHookInbound_SignedNormalizesWithThreadAndReply(t *testing.T) {
	t.Setenv("HOOK_SECRET", "s3cr3t")
	cfg := config.Transport{Kind: "hook", Account: "acct", TokenEnv: "HOOK_SECRET"}
	conn, err := newHookConn("hook", cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got envelope.Envelope
	h := conn.(Webhooker).Handler(func(e envelope.Envelope) { got = e })

	body := []byte(`{"text":"hello","sender":"muthu","thread_id":"chat-1","reply_to":"m-42"}`)
	req := httptest.NewRequest(http.MethodPost, "/hook/hook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", signHMAC([]byte("s3cr3t"), body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rec.Code)
	}
	if got.Text != "hello" || got.Sender != "muthu" || got.ThreadID != "chat-1" || got.ReplyTo != "m-42" {
		t.Fatalf("bad normalize: %+v", got)
	}

	bad := httptest.NewRequest(http.MethodPost, "/hook/hook", strings.NewReader(string(body)))
	bad.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, bad)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for bad sig, got %d", rec2.Code)
	}
}

// telegram send: a reply threads via reply_to_message_id.
func TestTelegramSend_ThreadsReply(t *testing.T) {
	t.Setenv("TG_TOKEN", "tok")
	var gotForm url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := config.Transport{Kind: "telegram", TokenEnv: "TG_TOKEN", Options: map[string]string{"baseURL": ts.URL}}
	env := envelope.Envelope{Channel: "telegram", Text: "hi", ThreadID: "999", ReplyTo: "42"}
	if err := (telegramSender{}).Send(context.Background(), env, cfg, NewSecretResolver(nil)); err != nil {
		t.Fatal(err)
	}
	if gotForm.Get("chat_id") != "999" || gotForm.Get("text") != "hi" || gotForm.Get("reply_to_message_id") != "42" {
		t.Fatalf("bad telegram form: %v", gotForm)
	}
}
