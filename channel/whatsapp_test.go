package channel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// errExit stands in for a wacli non-zero exit in send tests.
var errExit = errors.New("exit status 1")

// The whatsapp inbound WEBHOOK is the real transport (wacli sync stdout carries no
// messages): a signed POST to the receiver publishes a routed envelope; an unbound chat
// is dropped; our own from_me echo is dropped; a bad signature is 401'd.
func TestWhatsappStream_WebhookInboundRoutesDropsAndVerifies(t *testing.T) {
	t.Setenv("MESSENGER_HOME", t.TempDir())
	chans := map[string]config.Transport{
		"ops": {Kind: "whatsapp", Options: map[string]string{"group": "111@g.us"}},
	}
	st, err := openWhatsappStream(chans, NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*whatsappStream)
	if s.secret == "" {
		t.Fatal("stream should mint an inbound secret")
	}
	var got []envelope.Envelope
	h := s.Handler(func(e envelope.Envelope) { got = append(got, e) })

	post := func(body, sig string) int {
		req := httptest.NewRequest(http.MethodPost, s.Path(), strings.NewReader(body))
		if sig != "" {
			req.Header.Set("X-Wacli-Signature", sig)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	signed := func(body string) string { return SignHMAC([]byte(s.secret), []byte(body)) }

	// bound group, correctly signed → published, routed to "ops", threaded on the chat.
	b1 := `{"id":"WA1","chat":"111@g.us","sender":"muthu","text":"hello desk"}`
	if code := post(b1, signed(b1)); code != http.StatusOK {
		t.Fatalf("signed bound-group post: want 200, got %d", code)
	}
	if len(got) != 1 || got[0].Channel != "ops" || got[0].Text != "hello desk" ||
		got[0].ThreadID != "111@g.us" || got[0].ID != "WA1" {
		t.Fatalf("bad published envelope: %+v", got)
	}

	// unbound chat → dropped (no catch-all).
	b2 := `{"id":"WA2","chat":"999@g.us","sender":"x","text":"stray"}`
	post(b2, signed(b2))
	// our own echo → dropped.
	b3 := `{"id":"WA3","chat":"111@g.us","sender":"me","text":"my own send","from_me":true}`
	post(b3, signed(b3))
	if len(got) != 1 {
		t.Fatalf("unbound + from_me must be dropped, got %d envelopes: %+v", len(got), got)
	}

	// bad signature → 401, nothing published.
	if code := post(b1, "sha256=deadbeef"); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: want 401, got %d", code)
	}
	if len(got) != 1 {
		t.Fatalf("401 must not publish, got %d", len(got))
	}
}

// The webhook body may be a single object, a {messages:[…]} wrapper, or a bare array —
// messageItems teases each message out.
func TestWhatsappMessageItems_Shapes(t *testing.T) {
	cases := map[string]int{
		`{"id":"a","chat":"1","text":"x"}`:     1,
		`{"messages":[{"id":"a"},{"id":"b"}]}`: 2,
		`[{"id":"a"},{"id":"b"},{"id":"c"}]`:   3,
		`{"message":{"id":"a","text":"x"}}`:    1,
		`{"data":[{"id":"a"},{"id":"b"}]}`:     2,
	}
	for body, want := range cases {
		if got := len(messageItems([]byte(body))); got != want {
			t.Fatalf("messageItems(%s) = %d, want %d", body, got, want)
		}
	}
}

// fakeRunCmd records every wacli invocation and plays back canned outputs, so send and
// download paths are tested without a wacli binary.
type fakeRunCmd struct {
	calls [][]string
	out   []byte
	err   error
}

func (f *fakeRunCmd) run(_ context.Context, bin string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{bin}, args...))
	return f.out, f.err
}

func argsHavePair(args []string, flag, want string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == want {
			return true
		}
	}
	return false
}

func argsContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// whatsapp send with an attachment goes via `wacli send file` — --to/--file/--caption/
// --reply-to are wired through and the provider id from the JSON response is returned.
func TestWhatsappSend_AttachmentUsesSendFile(t *testing.T) {
	fake := &fakeRunCmd{out: []byte(`{"success":true,"data":{"id":"WAMID1"},"error":null}`)}
	ch := &whatsappChannel{
		name:   "ops",
		cfg:    config.Transport{Kind: "whatsapp", Options: map[string]string{"group": "111@g.us"}},
		runCmd: fake.run,
	}
	env := envelope.Envelope{
		Channel: "ops",
		Text:    "the caption",
		ReplyTo: "MSG9",
		Attachments: []envelope.Attachment{
			{Type: "image", Name: "pic.png", MIME: "image/png", Path: "/tmp/pic.png"},
		},
	}
	id, err := ch.Send(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if id != "WAMID1" {
		t.Fatalf("want provider id WAMID1, got %q", id)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("want 1 wacli call, got %d", len(fake.calls))
	}
	args := fake.calls[0]
	if !argsContain(args, "send") || !argsContain(args, "file") {
		t.Fatalf("want `send file` invocation, got %v", args)
	}
	if !argsHavePair(args, "--to", "111@g.us") {
		t.Fatalf("want --to 111@g.us (group default), got %v", args)
	}
	if !argsHavePair(args, "--file", "/tmp/pic.png") {
		t.Fatalf("want --file /tmp/pic.png, got %v", args)
	}
	if !argsHavePair(args, "--filename", "pic.png") || !argsHavePair(args, "--mime", "image/png") {
		t.Fatalf("want --filename/--mime from attachment, got %v", args)
	}
	if !argsHavePair(args, "--caption", "the caption") {
		t.Fatalf("want --caption from env.Text, got %v", args)
	}
	if !argsHavePair(args, "--reply-to", "MSG9") {
		t.Fatalf("want --reply-to MSG9, got %v", args)
	}
}

// a "voice" attachment adds --ptt so wacli renders it as a WhatsApp voice-note bubble
// instead of a generic file attachment; other attachment types must NOT get --ptt.
func TestWhatsappSend_VoiceAttachmentUsesPTT(t *testing.T) {
	fake := &fakeRunCmd{out: []byte(`{"success":true,"data":{"id":"WAMID3"},"error":null}`)}
	ch := &whatsappChannel{
		name:   "ops",
		cfg:    config.Transport{Kind: "whatsapp", Options: map[string]string{"group": "111@g.us"}},
		runCmd: fake.run,
	}
	env := envelope.Envelope{
		Channel: "ops",
		Attachments: []envelope.Attachment{
			{Type: "voice", MIME: "audio/ogg", Path: "/tmp/reply.ogg"},
		},
	}
	if _, err := ch.Send(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if !argsContain(fake.calls[0], "--ptt") {
		t.Fatalf("want --ptt for a voice attachment, got %v", fake.calls[0])
	}

	fake2 := &fakeRunCmd{out: []byte(`{"success":true,"data":{"id":"WAMID4"},"error":null}`)}
	ch2 := &whatsappChannel{
		name:   "ops",
		cfg:    config.Transport{Kind: "whatsapp", Options: map[string]string{"group": "111@g.us"}},
		runCmd: fake2.run,
	}
	env2 := envelope.Envelope{
		Channel:     "ops",
		Attachments: []envelope.Attachment{{Type: "audio", Path: "/tmp/song.mp3"}},
	}
	if _, err := ch2.Send(context.Background(), env2); err != nil {
		t.Fatal(err)
	}
	if argsContain(fake2.calls[0], "--ptt") {
		t.Fatalf("non-voice attachment must NOT get --ptt: %v", fake2.calls[0])
	}
}

// env.Text rides as the caption on the FIRST attachment only.
func TestWhatsappSend_CaptionOnFirstAttachmentOnly(t *testing.T) {
	fake := &fakeRunCmd{out: []byte(`{"success":true,"data":{"id":"WAMID1"},"error":null}`)}
	ch := &whatsappChannel{
		name:   "ops",
		cfg:    config.Transport{Kind: "whatsapp", Options: map[string]string{"group": "111@g.us"}},
		runCmd: fake.run,
	}
	env := envelope.Envelope{
		Channel: "ops",
		Text:    "cap",
		Attachments: []envelope.Attachment{
			{Type: "image", Path: "/tmp/a.png"},
			{Type: "document", Path: "/tmp/b.pdf"},
		},
	}
	id, err := ch.Send(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if id != "WAMID1" {
		t.Fatalf("want first id WAMID1, got %q", id)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("want 2 wacli calls, got %d", len(fake.calls))
	}
	if !argsHavePair(fake.calls[0], "--caption", "cap") {
		t.Fatalf("first send should carry the caption: %v", fake.calls[0])
	}
	if argsContain(fake.calls[1], "--caption") {
		t.Fatalf("second send must NOT carry the caption: %v", fake.calls[1])
	}
	if !argsHavePair(fake.calls[1], "--file", "/tmp/b.pdf") {
		t.Fatalf("second send should carry the second file: %v", fake.calls[1])
	}
}

// plain text still goes via `send text`, threading with --reply-to (NOT --quote).
func TestWhatsappSend_TextUsesReplyToFlag(t *testing.T) {
	fake := &fakeRunCmd{out: []byte(`{"success":true,"data":{"id":"WAMID2"},"error":null}`)}
	ch := &whatsappChannel{
		name:   "ops",
		cfg:    config.Transport{Kind: "whatsapp", Options: map[string]string{"group": "111@g.us"}},
		runCmd: fake.run,
	}
	env := envelope.Envelope{Channel: "ops", Text: "hi", ReplyTo: "MSG7"}
	id, err := ch.Send(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if id != "WAMID2" {
		t.Fatalf("want provider id WAMID2, got %q", id)
	}
	args := fake.calls[0]
	if !argsContain(args, "text") || !argsHavePair(args, "--message", "hi") {
		t.Fatalf("want `send text --message hi`, got %v", args)
	}
	if !argsHavePair(args, "--reply-to", "MSG7") {
		t.Fatalf("want --reply-to MSG7, got %v", args)
	}
	if argsContain(args, "--quote") {
		t.Fatalf("--quote is not a wacli flag; got %v", args)
	}
}

// A group reply to a message wacli hasn't synced fails with "--reply-to-sender is required
// for unsynced group replies"; Send must retry ONCE adding --reply-to-sender <env.Sender>
// (the envelope carries it) and succeed — the fix for the flaky threaded-reply 502s.
func TestWhatsappSend_UnsyncedGroupReplyRetriesWithSender(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, bin string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{bin}, args...))
		if len(calls) == 1 {
			return []byte(`{"error":"--reply-to-sender is required for unsynced group replies"}`), errExit
		}
		return []byte(`{"success":true,"data":{"id":"WAMID9"}}`), nil
	}
	ch := &whatsappChannel{
		name:   "ops",
		cfg:    config.Transport{Kind: "whatsapp", Options: map[string]string{"group": "111@g.us"}},
		runCmd: run,
	}
	env := envelope.Envelope{Channel: "ops", Text: "on it", ReplyTo: "MSG7", Sender: "1555@s.whatsapp.net"}
	id, err := ch.Send(context.Background(), env)
	if err != nil {
		t.Fatalf("send should succeed after the reply-to-sender fallback: %v", err)
	}
	if id != "WAMID9" {
		t.Fatalf("want id WAMID9 from the retry, got %q", id)
	}
	if len(calls) != 2 {
		t.Fatalf("want exactly one retry (2 calls), got %d", len(calls))
	}
	if argsContain(calls[0], "--reply-to-sender") {
		t.Fatalf("first attempt must NOT carry --reply-to-sender: %v", calls[0])
	}
	if !argsHavePair(calls[1], "--reply-to-sender", "1555@s.whatsapp.net") {
		t.Fatalf("retry must add --reply-to-sender <sender>: %v", calls[1])
	}
	if !argsHavePair(calls[1], "--reply-to", "MSG7") {
		t.Fatalf("retry must keep --reply-to MSG7: %v", calls[1])
	}
}

// A non-reply send that fails is NOT retried (the fallback is reply-only).
func TestWhatsappSend_NonReplyFailureNotRetried(t *testing.T) {
	var n int
	run := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		n++
		return []byte(`{"error":"boom"}`), errExit
	}
	ch := &whatsappChannel{name: "ops", cfg: config.Transport{Options: map[string]string{"group": "111@g.us"}}, runCmd: run}
	if _, err := ch.Send(context.Background(), envelope.Envelope{Channel: "ops", Text: "hi"}); err == nil {
		t.Fatal("want error")
	}
	if n != 1 {
		t.Fatalf("non-reply send must not retry, got %d calls", n)
	}
}

// The delivered-send-never-retries invariant: a caller whose OWN context is already
// cancelled (an impatient CLI wrapper's short deadline, an HTTP client that walked away)
// must not abort a wacli dispatch that may already be in flight to WhatsApp. Send/sendFiles
// detach the wacli invocation from the caller's context (dispatchContext), so the fake
// runCmd here must still be called with a live, non-cancelled context and must still run to
// completion exactly once — proving a caller-side cancellation can never turn a real send
// into a false failure (the false failure is what invites a caller retry and the resulting
// duplicate delivery).
func TestWhatsappSend_SurvivesCallerContextCancellation(t *testing.T) {
	callerCtx, cancel := context.WithCancel(context.Background())
	cancel() // caller already gave up before Send is even invoked

	var calls int
	run := func(runCtx context.Context, _ string, _ ...string) ([]byte, error) {
		calls++
		if err := runCtx.Err(); err != nil {
			t.Fatalf("wacli dispatch context must be detached from the caller's, got already-done: %v", err)
		}
		return []byte(`{"success":true,"data":{"id":"WAMID-SURVIVED"}}`), nil
	}

	t.Run("text", func(t *testing.T) {
		calls = 0
		ch := &whatsappChannel{name: "ops", cfg: config.Transport{Options: map[string]string{"group": "111@g.us"}}, runCmd: run}
		id, err := ch.Send(callerCtx, envelope.Envelope{Channel: "ops", Text: "hi"})
		if err != nil {
			t.Fatalf("send must survive a cancelled caller context, got: %v", err)
		}
		if id != "WAMID-SURVIVED" || calls != 1 {
			t.Fatalf("want exactly 1 dispatched send, got id=%q calls=%d", id, calls)
		}
	})

	t.Run("file", func(t *testing.T) {
		calls = 0
		ch := &whatsappChannel{name: "ops", cfg: config.Transport{Options: map[string]string{"group": "111@g.us"}}, runCmd: run}
		env := envelope.Envelope{
			Channel:     "ops",
			Attachments: []envelope.Attachment{{Type: "voice", Name: "note.ogg", Path: "/tmp/note.ogg"}},
		}
		id, err := ch.Send(callerCtx, env)
		if err != nil {
			t.Fatalf("send must survive a cancelled caller context, got: %v", err)
		}
		if id != "WAMID-SURVIVED" || calls != 1 {
			t.Fatalf("want exactly 1 dispatched send (never a caller-cancellation-provoked retry), got id=%q calls=%d", id, calls)
		}
	})
}

// a stream media line publishes ONE envelope: text falls back to the caption, the
// attachment carries normalized type + name + mime, and a successful `wacli media
// download` fills Path (+ Size best-effort).
func TestWhatsappStream_MediaLinePublishesAttachment(t *testing.T) {
	mediaHome := t.TempDir()
	t.Setenv("MESSENGER_HOME", mediaHome)
	// A real file so os.Stat fills Size.
	stored := filepath.Join(mediaHome, "media", "pic.jpg")
	if err := os.MkdirAll(filepath.Dir(stored), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored, []byte("jpegbytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	chans := map[string]config.Transport{
		"ops": {Kind: "whatsapp", Account: "acct", Options: map[string]string{"group": "111@g.us"}},
	}
	st, err := openWhatsappStream(chans, NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*whatsappStream)
	fake := &fakeRunCmd{out: []byte(`{"success":true,"data":{"path":"` + stored + `"},"error":null}`)}
	s.runCmd = fake.run

	var got []envelope.Envelope
	line := `{"id":"M1","chat":"111@g.us","sender":"muthu","text":"","media_type":"image","caption":"look at this","filename":"pic.jpg","mime":"image/jpeg"}`
	s.ingest(context.Background(), []byte(line), func(e envelope.Envelope) { got = append(got, e) })

	if len(got) != 1 {
		t.Fatalf("want 1 published envelope, got %d", len(got))
	}
	e := got[0]
	if e.Text != "look at this" {
		t.Fatalf("text should fall back to the caption, got %q", e.Text)
	}
	if e.Channel != "ops" || e.ID != "M1" || e.ThreadID != "111@g.us" {
		t.Fatalf("bad routing/identity: %+v", e)
	}
	if len(e.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(e.Attachments))
	}
	a := e.Attachments[0]
	if a.Type != "image" || a.Name != "pic.jpg" || a.MIME != "image/jpeg" {
		t.Fatalf("bad attachment metadata: %+v", a)
	}
	if a.Path != stored {
		t.Fatalf("want downloaded path %q, got %q", stored, a.Path)
	}
	if a.Size != int64(len("jpegbytes")) {
		t.Fatalf("want stat'd size, got %d", a.Size)
	}
	// The download call went through wacli media download against the media dir.
	if len(fake.calls) != 1 {
		t.Fatalf("want 1 download call, got %d", len(fake.calls))
	}
	dl := fake.calls[0]
	if !argsContain(dl, "media") || !argsContain(dl, "download") ||
		!argsHavePair(dl, "--chat", "111@g.us") || !argsHavePair(dl, "--id", "M1") {
		t.Fatalf("bad download invocation: %v", dl)
	}
}

// a failed media download must NEVER drop the message: the envelope still publishes
// with a metadata-only attachment. PascalCase store fields (MediaType/MediaCaption/
// Filename/MimeType) parse too, and ptt normalizes to "voice".
func TestWhatsappStream_DownloadFailureStillPublishesMetadata(t *testing.T) {
	t.Setenv("MESSENGER_HOME", t.TempDir())
	chans := map[string]config.Transport{"grp": {Kind: "whatsapp", Options: map[string]string{"group": "555@g.us"}}}
	st, err := openWhatsappStream(chans, NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*whatsappStream)
	fake := &fakeRunCmd{out: []byte("boom"), err: os.ErrNotExist}
	s.runCmd = fake.run

	var got []envelope.Envelope
	line := `{"id":"M2","chat":"555@g.us","sender":"muthu","MediaType":"ptt","MediaCaption":"","Filename":"note.ogg","MimeType":"audio/ogg"}`
	s.ingest(context.Background(), []byte(line), func(e envelope.Envelope) { got = append(got, e) })

	if len(got) != 1 {
		t.Fatalf("download failure must still publish; got %d envelopes", len(got))
	}
	a := got[0].Attachments
	if len(a) != 1 || a[0].Type != "voice" || a[0].Name != "note.ogg" || a[0].MIME != "audio/ogg" {
		t.Fatalf("bad metadata-only attachment: %+v", a)
	}
	if a[0].Path != "" || a[0].Size != 0 {
		t.Fatalf("failed download must leave Path/Size empty: %+v", a[0])
	}
}

// a line with neither text nor media (delivery receipts etc.) publishes nothing.
func TestWhatsappStream_EmptyLineNotPublished(t *testing.T) {
	t.Setenv("MESSENGER_HOME", t.TempDir())
	chans := map[string]config.Transport{"home": {Kind: "whatsapp"}}
	st, err := openWhatsappStream(chans, NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*whatsappStream)
	published := 0
	pub := func(envelope.Envelope) { published++ }
	s.ingest(context.Background(), []byte(`{"id":"M3","chat":"111@g.us","sender":"x"}`), pub)
	s.ingest(context.Background(), []byte(`not json`), pub)
	s.ingest(context.Background(), []byte(``), pub)
	if published != 0 {
		t.Fatalf("want 0 published, got %d", published)
	}
}

// --- store-lock self-heal (crash-loop root cause) ---

// TestStoreLockedRe_ExtractsPID: the regex pulls the offending PID out of wacli's real
// store-locked error line (the exact JSON wacli 0.11.1 emits on stdout).
func TestStoreLockedRe_ExtractsPID(t *testing.T) {
	line := `{"event":"error","data":{"message":"store is locked (another wacli is running?): store locked: resource temporarily unavailable (pid=91904\nacquired_at=2026-07-09T15:48:39+05:30)"},"ts":1783593194053}`
	mm := storeLockedRe.FindStringSubmatch(line)
	if mm == nil {
		t.Fatal("expected the store-locked line to match")
	}
	if mm[1] != "91904" {
		t.Fatalf("pid = %q, want 91904", mm[1])
	}
}

// TestReapStrayWacli_Guards: never reaps ourselves, a bogus pid, or a non-wacli process
// (proven by spawning a real `sleep` and confirming it is left alive).
func TestReapStrayWacli_Guards(t *testing.T) {
	if reapStrayWacli(0) || reapStrayWacli(-1) {
		t.Fatal("must not reap a non-positive pid")
	}
	if reapStrayWacli(os.Getpid()) {
		t.Fatal("must never reap our own process")
	}
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Skipf("cannot spawn sleep: %v", err)
	}
	defer func() { _ = sleep.Process.Kill() }()
	if reapStrayWacli(sleep.Process.Pid) {
		t.Fatal("must not reap a non-wacli process")
	}
	if sleep.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("the non-wacli process should still be alive")
	}
}

// TestRunSync_ReportsStoreLockPID: a fake wacli that prints the store-locked line and
// exits non-zero is observed by runSync, which returns the exit error AND the pid to reap
// — the input the supervisor needs to self-heal instead of blind-looping.
func TestRunSync_ReportsStoreLockPID(t *testing.T) {
	s := &whatsappStream{
		bin:            "irrelevant",
		commandContext: fakeWacliStoreLocked(t, 4242),
	}
	err, pid := s.runSync(context.Background(), []string{"sync"})
	if err == nil {
		t.Fatal("expected a non-nil exit error from the locked wacli")
	}
	if pid != 4242 {
		t.Fatalf("lock pid = %d, want 4242", pid)
	}
}

// fakeWacliStoreLocked returns a commandContext that re-execs the test binary into
// TestHelperProcess, which prints the store-locked line for the given pid and exits 1.
func fakeWacliStoreLocked(t *testing.T, pid int) func(ctx context.Context, name string, arg ...string) *exec.Cmd {
	return func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"HELPER_LOCK_PID="+strconv.Itoa(pid),
		)
		return cmd
	}
}

// TestHelperProcess is not a real test: it is the fake wacli subprocess.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	fmt.Printf(`{"event":"error","data":{"message":"store is locked (another wacli is running?): resource temporarily unavailable (pid=%s\nacquired_at=x)"},"ts":1}`+"\n", os.Getenv("HELPER_LOCK_PID"))
	os.Exit(1)
}

// --- live wacli --webhook shape: media is a NESTED object, not a flat media_type ---

// TestWhatsappStream_NestedMediaWebhookDownloads reproduces the EXACT shape wacli's
// `sync --follow --webhook` posts for a voice note (top-level PascalCase Chat/ID/Text and
// a nested "Media" object — matched case-insensitively by encoding/json). The old parser
// only looked for a flat media_type, so it created no attachment and never downloaded;
// the message landed as "[Audio]" with attachments=null. Now the nested Media triggers
// downloadMedia and populates attachments[0].path.
func TestWhatsappStream_NestedMediaWebhookDownloads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MESSENGER_HOME", dir)
	stored := filepath.Join(dir, "message-XYZ.oga")
	if err := os.WriteFile(stored, []byte("oggopusbytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	chans := map[string]config.Transport{"muthu": {Kind: "whatsapp", Options: map[string]string{"group": "120363408634625681@g.us"}}}
	st, err := openWhatsappStream(chans, NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*whatsappStream)
	fake := &fakeRunCmd{out: []byte(`{"success":true,"data":{"path":"` + stored + `"},"error":null}`)}
	s.runCmd = fake.run

	// The real top-level webhook shape, with a nested Media object for a voice note.
	line := `{"Chat":"120363408634625681@g.us","ID":"ACE201EDC9DB4914676B115A0F5A6186","SenderJID":"181127444713663@lid","FromMe":false,"Text":"[Audio]","Media":{"kind":"audio","mimetype":"audio/ogg; codecs=opus","name":"note.oga"},"PushName":"Muthu"}`
	var got []envelope.Envelope
	s.ingest(context.Background(), []byte(line), func(e envelope.Envelope) { got = append(got, e) })

	if len(got) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(got))
	}
	a := got[0].Attachments
	if len(a) != 1 {
		t.Fatalf("nested Media must yield 1 attachment, got %d (bug: flat-only parse)", len(a))
	}
	if a[0].Type != "audio" || a[0].Path != stored || a[0].MIME != "audio/ogg; codecs=opus" {
		t.Fatalf("bad attachment from nested Media: %+v", a[0])
	}
	if a[0].Size != int64(len("oggopusbytes")) {
		t.Fatalf("attachment size not stat'd: %+v", a[0])
	}
	if len(fake.calls) != 1 || !argsHavePair(fake.calls[0], "--chat", "120363408634625681@g.us") ||
		!argsHavePair(fake.calls[0], "--id", "ACE201EDC9DB4914676B115A0F5A6186") {
		t.Fatalf("downloadMedia not invoked with chat+id: %v", fake.calls)
	}
}

// TestWhatsappStream_OpaqueMediaStillDownloads: even if wacli's Media inner shape is one
// we don't recognize, a non-null Media object must still be treated as an attachment
// (type "file") and downloaded by id — a voice note is never silently dropped to text.
func TestWhatsappStream_OpaqueMediaStillDownloads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MESSENGER_HOME", dir)
	stored := filepath.Join(dir, "blob.bin")
	_ = os.WriteFile(stored, []byte("x"), 0o600)
	chans := map[string]config.Transport{"g": {Kind: "whatsapp", Options: map[string]string{"group": "1@g.us"}}}
	st, _ := openWhatsappStream(chans, NewSecretResolver(nil))
	s := st.(*whatsappStream)
	fake := &fakeRunCmd{out: []byte(`{"success":true,"data":{"path":"` + stored + `"}}`)}
	s.runCmd = fake.run

	line := `{"chat":"1@g.us","id":"OP1","sender":"x","Media":{"some":"future","shape":123}}`
	var got []envelope.Envelope
	s.ingest(context.Background(), []byte(line), func(e envelope.Envelope) { got = append(got, e) })
	if len(got) != 1 || len(got[0].Attachments) != 1 {
		t.Fatalf("opaque Media must still attach+publish, got %+v", got)
	}
	if got[0].Attachments[0].Type != "file" || got[0].Attachments[0].Path != stored {
		t.Fatalf("opaque Media should download as file: %+v", got[0].Attachments[0])
	}
}

// TestWhatsappStream_NullMediaIsTextOnly: a plain text message (Media:null) must NOT
// become an attachment.
func TestWhatsappStream_NullMediaIsTextOnly(t *testing.T) {
	t.Setenv("MESSENGER_HOME", t.TempDir())
	chans := map[string]config.Transport{"g": {Kind: "whatsapp", Options: map[string]string{"group": "1@g.us"}}}
	st, _ := openWhatsappStream(chans, NewSecretResolver(nil))
	s := st.(*whatsappStream)
	fake := &fakeRunCmd{out: []byte(`{}`)}
	s.runCmd = fake.run
	line := `{"Chat":"1@g.us","ID":"T1","SenderJID":"x","Text":"hello","Media":null}`
	var got []envelope.Envelope
	s.ingest(context.Background(), []byte(line), func(e envelope.Envelope) { got = append(got, e) })
	if len(got) != 1 || len(got[0].Attachments) != 0 {
		t.Fatalf("text with Media:null must have no attachment: %+v", got)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("must not attempt a download for a text message: %v", fake.calls)
	}
}
