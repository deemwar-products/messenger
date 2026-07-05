package channel

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

// webhook inbound: a correctly-signed POST normalizes to an Envelope carrying thread +
// reply; a bad signature is rejected 401. The legacy kind name "hook" resolves to the
// same channel.
func TestWebhookInbound_SignedNormalizesWithThreadAndReply(t *testing.T) {
	t.Setenv("HOOK_SECRET", "s3cr3t")
	cfg := config.Transport{Kind: "hook", Account: "acct", TokenEnv: "HOOK_SECRET"}
	k, err := KindFor("in", cfg)
	if err != nil || k.Name() != "webhook" {
		t.Fatalf("legacy kind hook should resolve to webhook: %v %v", k, err)
	}
	ch, err := k.Open("in", cfg, NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	var got envelope.Envelope
	h := ch.(Pushed).Handler(func(e envelope.Envelope) { got = e })

	body := []byte(`{"text":"hello","sender":"muthu","thread_id":"chat-1","reply_to":"m-42"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/in", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", SignHMAC([]byte("s3cr3t"), body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rec.Code)
	}
	if got.Text != "hello" || got.Sender != "muthu" || got.ThreadID != "chat-1" || got.ReplyTo != "m-42" {
		t.Fatalf("bad normalize: %+v", got)
	}

	bad := httptest.NewRequest(http.MethodPost, "/webhook/in", strings.NewReader(string(body)))
	bad.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, bad)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for bad sig, got %d", rec2.Code)
	}
}

// telegram send: a reply threads via reply_to_message_id, the configured chatId is the
// default target, and the provider message_id is returned.
func TestTelegramSend_ThreadsReplyAndReturnsID(t *testing.T) {
	t.Setenv("TG_TOKEN", "tok")
	var gotForm url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":777}}`))
	}))
	defer ts.Close()

	cfg := config.Transport{Kind: "telegram", TokenEnv: "TG_TOKEN",
		Options: map[string]string{"baseURL": ts.URL, "chatId": "999"}}
	ch, err := openTelegram("mybot", cfg, NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	env := envelope.Envelope{Channel: "mybot", Text: "hi", ReplyTo: "42"} // no ThreadID → chatId default
	id, err := ch.Send(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if gotForm.Get("chat_id") != "999" || gotForm.Get("text") != "hi" || gotForm.Get("reply_to_message_id") != "42" {
		t.Fatalf("bad telegram form: %v", gotForm)
	}
	if id != "777" {
		t.Fatalf("want provider id 777, got %q", id)
	}
}

// whatsapp: ONE shared stream serves many group channels — inbound is routed to the
// channel whose group JID matches the chat, and unmatched chats land on the catch-all.
func TestWhatsappStream_RoutesGroupsToChannels(t *testing.T) {
	chans := map[string]config.Transport{
		"ops":  {Kind: "whatsapp", Options: map[string]string{"group": "111@g.us"}},
		"fam":  {Kind: "whatsapp", Options: map[string]string{"group": "222@g.us"}},
		"home": {Kind: "whatsapp"}, // no group = catch-all
	}
	st, err := openWhatsappStream(chans, NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*whatsappStream)
	if got := s.route("111@g.us"); got != "ops" {
		t.Fatalf("111 should route to ops, got %q", got)
	}
	if got := s.route("222@g.us"); got != "fam" {
		t.Fatalf("222 should route to fam, got %q", got)
	}
	if got := s.route("someone@s.whatsapp.net"); got != "home" {
		t.Fatalf("unmatched should route to catch-all home, got %q", got)
	}
}

// runtime: only ONE whatsapp stream starts no matter how many whatsapp channels are
// configured, and Send routes by channel name.
func TestRuntime_SingleWhatsappStreamAndSendRouting(t *testing.T) {
	t.Setenv("HOOK_SECRET", "s")
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer cb.Close()

	seed := map[string]config.Transport{
		"ops": {Kind: "whatsapp", Enabled: true, Options: map[string]string{
			"group": "111@g.us", "bin": "sh", "args": "-c sleep 3600"}},
		"fam": {Kind: "whatsapp", Enabled: true, Options: map[string]string{"group": "222@g.us"}},
		"in":  {Kind: "webhook", Enabled: true, TokenEnv: "HOOK_SECRET", Options: map[string]string{"callbackURL": cb.URL}},
	}
	rt := NewRuntime(seed, NewSecretResolver(nil), func(envelope.Envelope) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Up(ctx); err != nil {
		t.Fatal(err)
	}
	defer rt.Down()

	ks := rt.Channels()
	if len(ks) != 3 || ks["ops"] != "whatsapp" || ks["in"] != "webhook" {
		t.Fatalf("bad channels: %v", ks)
	}
	// Send on the webhook channel returns the envelope id.
	id, err := rt.Send(ctx, envelope.Normalize(envelope.Envelope{Channel: "in", Text: "x"}))
	if err != nil || id == "" {
		t.Fatalf("webhook send: id=%q err=%v", id, err)
	}
	// Unknown channel errors.
	if _, err := rt.Send(ctx, envelope.Envelope{Channel: "nope", Text: "x"}); err == nil {
		t.Fatal("want error for unknown channel")
	}
}
