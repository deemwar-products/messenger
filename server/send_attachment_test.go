package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deemwar-products/messenger/channel"
	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/home"
	"github.com/deemwar-products/messenger/inbox"
)

// newTestSendServer stands up a hermetic server (webhook "out" channel delivering to a
// captured callback) with MESSENGER_HOME pinned to a fresh temp dir, and returns the
// running httptest server URL plus a pointer to the last delivered payload.
func newTestSendServer(t *testing.T) (baseURL string, delivered *map[string]any) {
	t.Helper()
	t.Setenv("MESSENGER_HOME", t.TempDir())
	t.Setenv("HOOK_SECRET", "s")

	delivered = &map[string]any{}
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		*delivered = got
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(cb.Close)

	cfg := &config.Config{Transports: map[string]config.Transport{
		"out": {Kind: "webhook", Enabled: true, TokenEnv: "HOOK_SECRET", Options: map[string]string{"callbackURL": cb.URL}},
	}}
	box, err := inbox.Open(filepath.Join(t.TempDir(), "inbox.ndjson"))
	if err != nil {
		t.Fatal(err)
	}

	rt := channel.NewRuntime(cfg.Enabled(), channel.NewSecretResolver(nil), func(envelope.Envelope) {})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Up(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Down)

	s := New(rt, box, "tok")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv.URL, delivered
}

func postSend(t *testing.T, baseURL, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/send", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// POST /send with `file` pointing OUTSIDE the media dir must be rejected with 400, and
// the underlying channel send must never fire.
func TestSend_FileOutsideMediaDirRejected(t *testing.T) {
	baseURL, delivered := newTestSendServer(t)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"channel": "out", "text": "x", "file": outside})
	resp := postSend(t, baseURL, string(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for outside-media-dir file, got %d", resp.StatusCode)
	}
	if len(*delivered) != 0 {
		t.Fatalf("channel send should never fire for a rejected attachment, got %v", *delivered)
	}
}

// A classic path-traversal target (e.g. /etc/passwd) must also be rejected.
func TestSend_FileAbsoluteSystemPathRejected(t *testing.T) {
	baseURL, delivered := newTestSendServer(t)

	body, _ := json.Marshal(map[string]any{"channel": "out", "text": "x", "file": "/etc/passwd"})
	resp := postSend(t, baseURL, string(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for /etc/passwd, got %d", resp.StatusCode)
	}
	if len(*delivered) != 0 {
		t.Fatalf("channel send should never fire for a rejected attachment, got %v", *delivered)
	}
}

// POST /send with `file` pointing INSIDE the configured media dir must still succeed —
// the legitimate "forward what you received" case is not broken by the fix.
func TestSend_FileInsideMediaDirSucceeds(t *testing.T) {
	baseURL, delivered := newTestSendServer(t)

	mediaDir := home.MediaDir()
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(mediaDir, "photo.jpg")
	if err := os.WriteFile(inside, []byte("fake jpeg bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"channel": "out", "text": "here you go", "file": inside})
	resp := postSend(t, baseURL, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for in-media-dir file, got %d", resp.StatusCode)
	}
	if (*delivered)["text"] != "here you go" {
		t.Fatalf("attachment send should still deliver: %v", *delivered)
	}
	atts, _ := (*delivered)["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("want 1 delivered attachment, got %v", (*delivered)["attachments"])
	}
}

// A `..`-traversal attempt rooted inside the media dir but resolving outside it must be
// rejected too — the boundary check has to be a real path check, not a string prefix.
func TestSend_FileTraversalOutOfMediaDirRejected(t *testing.T) {
	baseURL, delivered := newTestSendServer(t)

	mediaDir := home.MediaDir()
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// mediaDir/../secret-outside.txt resolves to a sibling of the media dir, outside it.
	outside := filepath.Join(filepath.Dir(mediaDir), "secret-outside.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	traversal := filepath.Join(mediaDir, "..", "secret-outside.txt")

	body, _ := json.Marshal(map[string]any{"channel": "out", "text": "x", "file": traversal})
	resp := postSend(t, baseURL, string(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for traversal outside media dir, got %d", resp.StatusCode)
	}
	if len(*delivered) != 0 {
		t.Fatalf("channel send should never fire for a rejected attachment, got %v", *delivered)
	}
}

// A sibling directory that merely shares the media dir's name as a prefix (e.g.
// "<home>/media-evil" against "<home>/media") must NOT pass the check — guards against
// the classic strings.HasPrefix-without-separator bypass.
func TestSend_FilePrefixSiblingDirectoryRejected(t *testing.T) {
	baseURL, delivered := newTestSendServer(t)

	mediaDir := home.MediaDir()
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	evilDir := mediaDir + "-evil"
	if err := os.MkdirAll(evilDir, 0o755); err != nil {
		t.Fatal(err)
	}
	evilFile := filepath.Join(evilDir, "gotcha.txt")
	if err := os.WriteFile(evilFile, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"channel": "out", "text": "x", "file": evilFile})
	resp := postSend(t, baseURL, string(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for media-dir-prefix sibling, got %d", resp.StatusCode)
	}
	if len(*delivered) != 0 {
		t.Fatalf("channel send should never fire for a rejected attachment, got %v", *delivered)
	}
}

// The same restriction must apply when the path arrives directly in attachments[],
// not just via the `file` shorthand.
func TestSend_AttachmentsArrayPathOutsideMediaDirRejected(t *testing.T) {
	baseURL, delivered := newTestSendServer(t)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	req := map[string]any{
		"channel": "out",
		"text":    "x",
		"attachments": []map[string]any{
			{"type": "file", "path": outside},
		},
	}
	body, _ := json.Marshal(req)
	resp := postSend(t, baseURL, string(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for attachments[].path outside media dir, got %d", resp.StatusCode)
	}
	if len(*delivered) != 0 {
		t.Fatalf("channel send should never fire for a rejected attachment, got %v", *delivered)
	}
}

// attachments[].path INSIDE the media dir must still succeed, same as the file shorthand.
func TestSend_AttachmentsArrayPathInsideMediaDirSucceeds(t *testing.T) {
	baseURL, delivered := newTestSendServer(t)

	mediaDir := home.MediaDir()
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(mediaDir, "doc.pdf")
	if err := os.WriteFile(inside, []byte("fake pdf bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	req := map[string]any{
		"channel": "out",
		"text":    "here",
		"attachments": []map[string]any{
			{"type": "document", "path": inside},
		},
	}
	body, _ := json.Marshal(req)
	resp := postSend(t, baseURL, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for attachments[].path inside media dir, got %d", resp.StatusCode)
	}
	if (*delivered)["text"] != "here" {
		t.Fatalf("attachment send should still deliver: %v", *delivered)
	}
}

// A remote http(s) attachment (URL set, Path empty) is never subject to the media-dir
// check — that's the whole point of the URL field.
func TestSend_URLAttachmentUnaffected(t *testing.T) {
	baseURL, delivered := newTestSendServer(t)

	body, _ := json.Marshal(map[string]any{"channel": "out", "text": "x", "file": "https://example.com/y.pdf"})
	resp := postSend(t, baseURL, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for http(s) file, got %d", resp.StatusCode)
	}
	if (*delivered)["text"] != "x" {
		t.Fatalf("url attachment send should still deliver: %v", *delivered)
	}
}
