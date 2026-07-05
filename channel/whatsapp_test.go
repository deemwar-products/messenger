package channel

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

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
	s.handleLine(context.Background(), line, func(e envelope.Envelope) { got = append(got, e) })

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
	chans := map[string]config.Transport{"home": {Kind: "whatsapp"}}
	st, err := openWhatsappStream(chans, NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*whatsappStream)
	fake := &fakeRunCmd{out: []byte("boom"), err: os.ErrNotExist}
	s.runCmd = fake.run

	var got []envelope.Envelope
	line := `{"id":"M2","chat":"someone@s.whatsapp.net","sender":"muthu","MediaType":"ptt","MediaCaption":"","Filename":"note.ogg","MimeType":"audio/ogg"}`
	s.handleLine(context.Background(), line, func(e envelope.Envelope) { got = append(got, e) })

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
	s.handleLine(context.Background(), `{"id":"M3","chat":"111@g.us","sender":"x"}`, pub)
	s.handleLine(context.Background(), `not json`, pub)
	s.handleLine(context.Background(), ``, pub)
	if published != 0 {
		t.Fatalf("want 0 published, got %d", published)
	}
}
