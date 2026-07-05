package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// newTelegram builds a telegram channel pointed at a fake Bot API server.
func newTelegram(t *testing.T, baseURL string) Channel {
	t.Helper()
	t.Setenv("TG_TOKEN", "tok")
	cfg := config.Transport{Kind: "telegram", TokenEnv: "TG_TOKEN",
		Options: map[string]string{"baseURL": baseURL, "chatId": "999"}}
	ch, err := openTelegram("mybot", cfg, NewSecretResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

// inbound photo: the caption becomes the text, the LAST (largest) PhotoSize is chosen,
// the file is downloaded via getFile + the file endpoint into $MESSENGER_HOME/media as
// "<messageID>-<name>", and Path/Size land on the attachment.
func TestTelegramInbound_PhotoDownloadsToMediaDir(t *testing.T) {
	t.Setenv("MESSENGER_HOME", t.TempDir())
	content := []byte("jpeg-bytes")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/bottok/getFile":
			_ = r.ParseForm()
			if r.PostForm.Get("file_id") != "big" {
				t.Errorf("getFile should ask for the LAST photo size, got file_id=%q", r.PostForm.Get("file_id"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"photos/file_1.jpg"}}`))
		case r.URL.Path == "/file/bottok/photos/file_1.jpg":
			_, _ = w.Write(content)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	ch := newTelegram(t, ts.URL)
	var got envelope.Envelope
	h := ch.(Pushed).Handler(func(e envelope.Envelope) { got = e })

	update := `{"message":{"message_id":55,"caption":"look at this","chat":{"id":42},
		"from":{"id":7,"username":"muthu"},
		"photo":[{"file_id":"small","file_size":10},{"file_id":"big","file_size":100}]}}`
	req := httptest.NewRequest(http.MethodPost, "/telegram/mybot", strings.NewReader(update))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got.Text != "look at this" {
		t.Fatalf("caption should become text, got %q", got.Text)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(got.Attachments))
	}
	att := got.Attachments[0]
	if att.Type != "image" {
		t.Fatalf("want type image, got %q", att.Type)
	}
	wantPath := filepath.Join(os.Getenv("MESSENGER_HOME"), "media", "55-file_1.jpg")
	if att.Path != wantPath {
		t.Fatalf("want path %q, got %q", wantPath, att.Path)
	}
	if att.Size != int64(len(content)) {
		t.Fatalf("want size %d, got %d", len(content), att.Size)
	}
	onDisk, err := os.ReadFile(att.Path)
	if err != nil || string(onDisk) != string(content) {
		t.Fatalf("bad file on disk: %q err=%v", onDisk, err)
	}
}

// inbound download failure: getFile 500s, but the envelope is still published with the
// metadata-only attachment — a message is never dropped.
func TestTelegramInbound_DownloadFailureKeepsMetadataAttachment(t *testing.T) {
	t.Setenv("MESSENGER_HOME", t.TempDir())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	ch := newTelegram(t, ts.URL)
	var got envelope.Envelope
	h := ch.(Pushed).Handler(func(e envelope.Envelope) { got = e })

	update := `{"message":{"message_id":56,"caption":"the report","chat":{"id":42},
		"from":{"id":7,"username":"muthu"},
		"document":{"file_id":"doc-1","file_name":"report.pdf","mime_type":"application/pdf","file_size":2048}}}`
	req := httptest.NewRequest(http.MethodPost, "/telegram/mybot", strings.NewReader(update))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got.Text != "the report" || len(got.Attachments) != 1 {
		t.Fatalf("envelope must still publish: %+v", got)
	}
	att := got.Attachments[0]
	if att.Type != "document" || att.Name != "report.pdf" || att.MIME != "application/pdf" {
		t.Fatalf("bad metadata attachment: %+v", att)
	}
	if att.Path != "" {
		t.Fatalf("failed download must leave Path empty, got %q", att.Path)
	}
	if att.Size != 2048 {
		t.Fatalf("metadata size should survive, got %d", att.Size)
	}
}

// outbound Path attachment: an image goes to sendPhoto as a multipart upload with
// caption + chat_id, and the provider message_id is returned.
func TestTelegramSend_PathAttachmentUploadsMultipart(t *testing.T) {
	src := filepath.Join(t.TempDir(), "pic.png")
	if err := os.WriteFile(src, []byte("png-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotPath, gotCT string
	var gotForm url.Values
	var gotFile []byte
	var gotFilename string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			gotForm = r.MultipartForm.Value
			if fhs := r.MultipartForm.File["photo"]; len(fhs) == 1 {
				gotFilename = fhs[0].Filename
				f, _ := fhs[0].Open()
				buf := make([]byte, fhs[0].Size)
				_, _ = f.Read(buf)
				_ = f.Close()
				gotFile = buf
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":888}}`))
	}))
	defer ts.Close()

	ch := newTelegram(t, ts.URL)
	env := envelope.Envelope{Channel: "mybot", Text: "here you go", ReplyTo: "42",
		Attachments: []envelope.Attachment{{Type: "image", Name: "pic.png", Path: src}}}
	id, err := ch.Send(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bottok/sendPhoto" {
		t.Fatalf("image should hit sendPhoto, got %q", gotPath)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Fatalf("want multipart upload, got Content-Type %q", gotCT)
	}
	if got := gotForm["chat_id"]; len(got) != 1 || got[0] != "999" {
		t.Fatalf("bad chat_id: %v", gotForm)
	}
	if got := gotForm["caption"]; len(got) != 1 || got[0] != "here you go" {
		t.Fatalf("bad caption: %v", gotForm)
	}
	if got := gotForm["reply_to_message_id"]; len(got) != 1 || got[0] != "42" {
		t.Fatalf("bad reply_to_message_id: %v", gotForm)
	}
	if gotFilename != "pic.png" || string(gotFile) != "png-bytes" {
		t.Fatalf("bad file part: name=%q body=%q", gotFilename, gotFile)
	}
	if id != "888" {
		t.Fatalf("want provider id 888, got %q", id)
	}
}

// outbound URL attachment: no local Path, so the URL rides as a plain form string in
// the media field (Telegram fetches it server-side) — no multipart.
func TestTelegramSend_URLAttachmentPassesPlainForm(t *testing.T) {
	var gotPath, gotCT string
	var gotForm url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":5}}`))
	}))
	defer ts.Close()

	ch := newTelegram(t, ts.URL)
	env := envelope.Envelope{Channel: "mybot", Text: "read this",
		Attachments: []envelope.Attachment{{Type: "document", URL: "https://example.com/y.pdf"}}}
	id, err := ch.Send(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bottok/sendDocument" {
		t.Fatalf("document should hit sendDocument, got %q", gotPath)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Fatalf("URL attachment must be a plain form, got Content-Type %q", gotCT)
	}
	if gotForm.Get("document") != "https://example.com/y.pdf" {
		t.Fatalf("media field should carry the URL: %v", gotForm)
	}
	if gotForm.Get("caption") != "read this" || gotForm.Get("chat_id") != "999" {
		t.Fatalf("bad form: %v", gotForm)
	}
	if id != "5" {
		t.Fatalf("want provider id 5, got %q", id)
	}
}
