package server

import (
	"context"
	"encoding/json"
	"io"
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

// newMediaServer builds a Server over an empty runtime — enough surface for the /media
// endpoint, which reads only home.MediaDir().
func newMediaServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := &config.Config{Transports: map[string]config.Transport{}}
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
	srv := httptest.NewServer(New(rt, box, "tok").Handler())
	t.Cleanup(srv.Close)
	return srv
}

// GET /media/<basename> serves a stored file with the extension's content type, and is
// bearer-auth'd like the rest of the consumer API.
func TestMedia_ServesStoredFileWithAuth(t *testing.T) {
	t.Setenv("MESSENGER_HOME", t.TempDir())
	if err := os.MkdirAll(home.MediaDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.MediaDir(), "pic.png"), []byte("png-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newMediaServer(t)

	// Without the bearer → 401, no content.
	noauth, err := http.Get(srv.URL + "/media/pic.png")
	if err != nil {
		t.Fatal(err)
	}
	defer noauth.Body.Close()
	if noauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without bearer, got %d", noauth.StatusCode)
	}

	// With the bearer → the file, typed by extension.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/media/pic.png", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "png-bytes" {
		t.Fatalf("bad media response: code=%d body=%q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Fatalf("want image/png content type, got %q", ct)
	}

	// An absent file → 404.
	miss, _ := http.NewRequest(http.MethodGet, srv.URL+"/media/absent.bin", nil)
	miss.Header.Set("Authorization", "Bearer tok")
	mresp, err := http.DefaultClient.Do(miss)
	if err != nil {
		t.Fatal(err)
	}
	defer mresp.Body.Close()
	if mresp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for absent file, got %d", mresp.StatusCode)
	}
}

// /media never escapes the media dir: traversal shapes (raw dot-dot, escaped dot-dot,
// nested paths) are rejected with 400/404 and never leak a file outside media.
func TestMedia_RejectsPathTraversal(t *testing.T) {
	t.Setenv("MESSENGER_HOME", t.TempDir())
	if err := os.MkdirAll(home.MediaDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	// A sentinel OUTSIDE the media dir that must never be served.
	const sentinel = "sentinel-outside-media"
	if err := os.WriteFile(filepath.Join(home.Dir(), "config.toml"), []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newMediaServer(t)

	for _, path := range []string{
		"/media/../config.toml",
		"/media/%2e%2e%2fconfig.toml",
		"/media/..%2fconfig.toml",
		"/media/sub/dir",
		"/media/",
		"/media/.",
		"/media/..",
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: want 400/404, got %d (body %q)", path, resp.StatusCode, body)
		}
		if strings.Contains(string(body), sentinel) {
			t.Fatalf("%s: leaked file content outside the media dir", path)
		}
	}
}

// POST /send with the `file` shorthand attaches the local path: the webhook callback
// receives attachments[0] with path = the given file and name = its base.
func TestSend_FileShorthandRidesAsAttachment(t *testing.T) {
	t.Setenv("MESSENGER_HOME", t.TempDir())
	t.Setenv("HOOK_SECRET", "s")
	var delivered struct {
		Text        string                `json:"text"`
		Attachments []envelope.Attachment `json:"attachments"`
	}
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&delivered)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer cb.Close()

	cfg := &config.Config{Transports: map[string]config.Transport{
		"out": {Kind: "webhook", Enabled: true, TokenEnv: "HOOK_SECRET", Options: map[string]string{"callbackURL": cb.URL}},
	}}
	box, err := inbox.Open(filepath.Join(t.TempDir(), "inbox.ndjson"))
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
	srv := httptest.NewServer(New(rt, box, "tok").Handler())
	defer srv.Close()

	// The attachment must live under the media dir — POST /send only trusts local
	// paths inside home.MediaDir() (same boundary GET /media enforces).
	if err := os.MkdirAll(home.MediaDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home.MediaDir(), "report.pdf")
	if err := os.WriteFile(file, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"channel": "out", "file": file})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/send", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("send failed: err=%v code=%v", err, resp.StatusCode)
	}
	defer resp.Body.Close()
	var sent struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sent)
	if !sent.OK || sent.ID == "" {
		t.Fatalf("send should return an id: %+v", sent)
	}
	if len(delivered.Attachments) != 1 {
		t.Fatalf("want 1 attachment delivered, got %+v", delivered)
	}
	a := delivered.Attachments[0]
	if a.Path != file || a.Name != "report.pdf" || a.Type != "file" {
		t.Fatalf("bad delivered attachment: %+v", a)
	}
}
