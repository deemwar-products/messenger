package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/home"
)

// WhatsApp is GLOBAL: the host has ONE paired device (wacli's linked WhatsApp-Web
// session), and every configured whatsapp channel is a named GROUP on that device.
// Exactly one `wacli --json sync --follow` stream runs no matter how many whatsapp
// channels exist; inbound is routed to the channel whose options["group"] JID matches
// the message's chat, falling back to the catch-all channel (one configured with no
// group), else the first channel by name. Sends target the channel's group (or an
// explicit --to thread), threading via wacli --reply-to when ReplyTo is set.

// maxAttachmentFetch caps how much of a remote attachment URL we pull to a temp file
// before handing it to wacli (100 MB — beyond WhatsApp's own media ceiling anyway).
const maxAttachmentFetch = 100 << 20

// runCmdFunc is the exec seam shared by the send and stream paths: it runs bin with
// args and returns combined output, so tests fake wacli without a subprocess.
type runCmdFunc func(ctx context.Context, bin string, args ...string) ([]byte, error)

func defaultRunCmd(ctx context.Context, bin string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, bin, args...).CombinedOutput()
}

// whatsappChannel is one named group (or the catch-all) on the shared device.
type whatsappChannel struct {
	name string
	cfg  config.Transport

	// runCmd is the exec seam so tests fake wacli.
	runCmd runCmdFunc
}

func openWhatsapp(name string, cfg config.Transport, _ *SecretResolver) (Channel, error) {
	return &whatsappChannel{name: name, cfg: cfg, runCmd: defaultRunCmd}, nil
}

func (c *whatsappChannel) Name() string { return c.name }
func (c *whatsappChannel) Kind() string { return "whatsapp" }

// Send shells wacli. Target = explicit ThreadID, else the channel's group. A plain
// text envelope goes via `send text`; any attachments go via `send file` (one wacli
// call per attachment, env.Text as the caption on the first). The wacli send id is
// returned when its JSON output carries one.
func (c *whatsappChannel) Send(ctx context.Context, env envelope.Envelope) (string, error) {
	to := env.ThreadID
	if to == "" {
		to = c.cfg.Options["group"]
	}
	if to == "" {
		return "", fmt.Errorf("channel: whatsapp %q: no target (pass --to or configure --group <jid>)", c.name)
	}
	if len(env.Attachments) > 0 {
		return c.sendFiles(ctx, to, env)
	}
	args := []string{"--json", "send", "text", "--to", to, "--message", env.Text}
	if env.ReplyTo != "" {
		args = append(args, "--reply-to", env.ReplyTo)
	}
	out, err := c.runCmd(ctx, waBin(c.cfg), args...)
	if err != nil {
		return "", fmt.Errorf("channel: whatsapp: wacli send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseWacliSendID(out), nil
}

// sendFiles delivers env.Attachments via `wacli send file`, one call per attachment.
// A Path attachment is handed to wacli as-is; a URL-only attachment is fetched to a
// temp file first (size-capped) and removed after the send. env.Text rides as the
// caption on the FIRST attachment only; ReplyTo threads every send. Returns the first
// provider id wacli reports.
func (c *whatsappChannel) sendFiles(ctx context.Context, to string, env envelope.Envelope) (string, error) {
	bin := waBin(c.cfg)
	firstID := ""
	for i, att := range env.Attachments {
		path := att.Path
		var tmp string
		if path == "" && att.URL != "" {
			t, err := fetchToTemp(ctx, att.URL)
			if err != nil {
				return firstID, fmt.Errorf("channel: whatsapp: fetch attachment %d: %w", i, err)
			}
			path, tmp = t, t
		}
		if path == "" {
			return firstID, fmt.Errorf("channel: whatsapp: attachment %d has neither path nor url", i)
		}
		args := []string{"--json", "send", "file", "--to", to, "--file", path}
		if att.Name != "" {
			args = append(args, "--filename", att.Name)
		}
		if att.MIME != "" {
			args = append(args, "--mime", att.MIME)
		}
		if i == 0 && env.Text != "" {
			args = append(args, "--caption", env.Text)
		}
		if env.ReplyTo != "" {
			args = append(args, "--reply-to", env.ReplyTo)
		}
		out, err := c.runCmd(ctx, bin, args...)
		if tmp != "" {
			_ = os.Remove(tmp)
		}
		if err != nil {
			return firstID, fmt.Errorf("channel: whatsapp: wacli send file: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if firstID == "" {
			firstID = parseWacliSendID(out)
		}
	}
	return firstID, nil
}

// parseWacliSendID pulls the message id out of a wacli send JSON response
// ({"success":true,"data":{"id":...}} or data.message_id). Best-effort: "" when absent.
func parseWacliSendID(out []byte) string {
	var res struct {
		Data struct {
			ID        string `json:"id"`
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if json.Unmarshal(out, &res) == nil {
		if res.Data.ID != "" {
			return res.Data.ID
		}
		if res.Data.MessageID != "" {
			return res.Data.MessageID
		}
	}
	return ""
}

// fetchToTemp downloads url to a temp file (honoring ctx, capped at
// maxAttachmentFetch) and returns its path. The caller removes the file.
func fetchToTemp(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	f, err := os.CreateTemp("", "messenger-wa-*")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxAttachmentFetch+1))
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err == nil && n > maxAttachmentFetch {
		err = fmt.Errorf("GET %s: larger than %d bytes", url, int64(maxAttachmentFetch))
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// whatsappStream is the ONE shared inbound stream for every whatsapp channel: it runs
// wacli as a long-lived subprocess, reads its NDJSON message lines, routes each by chat
// JID to the matching group channel, and publishes the normalized Envelope. Supervised
// by the runtime (crash = backoff + restart).
type whatsappStream struct {
	bin  string
	args []string

	byGroup  map[string]string // group jid -> channel name
	catchAll string            // channel with no group (else first by name)
	accounts map[string]string // channel name -> account label

	// commandContext is the exec seam so tests inject a fake emitter.
	commandContext func(ctx context.Context, name string, arg ...string) *exec.Cmd
	// runCmd is the exec seam for one-shot wacli calls (media download).
	runCmd runCmdFunc
}

func openWhatsappStream(chans map[string]config.Transport, _ *SecretResolver) (Streamer, error) {
	if len(chans) == 0 {
		return nil, fmt.Errorf("channel: whatsapp stream: no channels")
	}
	names := make([]string, 0, len(chans))
	for n := range chans {
		names = append(names, n)
	}
	sort.Strings(names)

	s := &whatsappStream{
		byGroup:        map[string]string{},
		accounts:       map[string]string{},
		commandContext: exec.CommandContext,
		runCmd:         defaultRunCmd,
	}
	for _, n := range names {
		cfg := chans[n]
		if g := cfg.Options["group"]; g != "" {
			s.byGroup[g] = n
		} else if s.catchAll == "" {
			s.catchAll = n
		}
		s.accounts[n] = cfg.Account
		// bin/args overrides: first channel that sets them wins (they describe the ONE
		// device, not a channel).
		if s.bin == "" {
			s.bin = cfg.Options["bin"]
		}
		if len(s.args) == 0 && cfg.Options["args"] != "" {
			s.args = strings.Fields(cfg.Options["args"])
		}
	}
	if s.catchAll == "" {
		s.catchAll = names[0]
	}
	if s.bin == "" {
		s.bin = "wacli"
	}
	if len(s.args) == 0 {
		s.args = []string{"--json", "sync", "--follow"}
	}
	return s, nil
}

// waMessage is the slice of a wacli JSON message line we normalize. Media fields
// arrive in either casing depending on the wacli surface — the sync --follow stream
// emits compact lowercase keys while the local store (`messages list --json`) uses
// PascalCase — so both spellings are parsed and the accessors pick whichever is set.
type waMessage struct {
	ID     string `json:"id"`
	Chat   string `json:"chat"`
	Sender string `json:"sender"`
	Text   string `json:"text"`

	MediaTypeLC string `json:"media_type"`
	MediaTypePC string `json:"MediaType"`
	CaptionLC   string `json:"caption"`
	CaptionPC   string `json:"MediaCaption"`
	FilenameLC  string `json:"filename"`
	FilenamePC  string `json:"Filename"`
	MIMELC      string `json:"mime"`
	MIMEPC      string `json:"MimeType"`
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (m waMessage) mediaType() string { return firstNonEmpty(m.MediaTypeLC, m.MediaTypePC) }
func (m waMessage) caption() string   { return firstNonEmpty(m.CaptionLC, m.CaptionPC) }
func (m waMessage) filename() string  { return firstNonEmpty(m.FilenameLC, m.FilenamePC) }
func (m waMessage) mime() string      { return firstNonEmpty(m.MIMELC, m.MIMEPC) }

// attachmentType maps wacli's media type names onto envelope.Attachment.Type
// (image | video | audio | voice | document | file). ptt is WhatsApp's push-to-talk
// voice note. Anything unrecognized degrades to "file", never dropped.
func attachmentType(mediaType string) string {
	switch strings.ToLower(mediaType) {
	case "image":
		return "image"
	case "video":
		return "video"
	case "audio":
		return "audio"
	case "ptt", "voice":
		return "voice"
	case "document":
		return "document"
	default:
		return "file"
	}
}

// route returns the channel name a message in chat belongs to.
func (s *whatsappStream) route(chat string) string {
	if n, ok := s.byGroup[chat]; ok {
		return n
	}
	return s.catchAll
}

func (s *whatsappStream) Run(ctx context.Context, pub Publisher) error {
	cmd := s.commandContext(ctx, s.bin, s.args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("channel: whatsapp: start %q: %w", s.bin, err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		s.handleLine(ctx, scanner.Text(), pub)
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil // cancelled: clean stop
	}
	if waitErr != nil {
		return fmt.Errorf("channel: whatsapp: wacli exited: %w", waitErr)
	}
	return nil
}

// handleLine parses ONE stream line and publishes it when it carries a message: any
// text, or any media (with or without a caption). Media is downloaded to the local
// media dir best-effort; a failed download still publishes the envelope with a
// metadata-only attachment — an inbound message is never dropped over a fetch error.
func (s *whatsappStream) handleLine(ctx context.Context, line string, pub Publisher) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return // skip non-JSON progress lines
	}
	var m waMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return
	}
	if m.Text == "" && m.mediaType() == "" {
		return
	}
	text := m.Text
	if text == "" {
		text = m.caption()
	}
	name := s.route(m.Chat)
	env := envelope.Inbound(name, m.Sender, text, "WhatsApp")
	env.Account = s.accounts[name]
	if m.ID != "" {
		env.ID = m.ID // wacli's stable message id → reply/dedupe key
	}
	if m.Chat != "" {
		env.ThreadID = m.Chat
	}
	if mt := m.mediaType(); mt != "" {
		att := envelope.Attachment{
			Type: attachmentType(mt),
			Name: m.filename(),
			MIME: m.mime(),
		}
		if p := s.downloadMedia(ctx, m.Chat, m.ID); p != "" {
			att.Path = p
			if fi, err := os.Stat(p); err == nil {
				att.Size = fi.Size()
			}
		}
		env.Attachments = []envelope.Attachment{att}
	}
	pub(env)
}

// downloadMedia shells `wacli media download` into home.MediaDir() and returns the
// stored file's path, or "" on any failure (the caller publishes regardless). wacli
// serializes device access itself, so running inline on the stream is fine.
func (s *whatsappStream) downloadMedia(ctx context.Context, chat, id string) string {
	if chat == "" || id == "" {
		return ""
	}
	dir := home.MediaDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	out, err := s.runCmd(ctx, s.bin, "media", "download", "--chat", chat, "--id", id, "--output", dir, "--json")
	if err != nil {
		return ""
	}
	var res struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(out, &res) != nil || len(res.Data) == 0 {
		return ""
	}
	// data may be a bare path string or an object naming the file under one of a few keys.
	var bare string
	if json.Unmarshal(res.Data, &bare) == nil && bare != "" {
		return bare
	}
	var obj struct {
		Path      string `json:"path"`
		File      string `json:"file"`
		LocalPath string `json:"local_path"`
		Output    string `json:"output"`
	}
	if json.Unmarshal(res.Data, &obj) == nil {
		for _, p := range []string{obj.Path, obj.File, obj.LocalPath, obj.Output} {
			if p != "" {
				return p
			}
		}
	}
	return ""
}

func waBin(cfg config.Transport) string {
	if b := cfg.Options["bin"]; b != "" {
		return b
	}
	return "wacli"
}

// DeviceStatus is the host's global WhatsApp pair state, read from `wacli doctor`.
type DeviceStatus struct {
	Installed     bool
	Authenticated bool
	LinkedJID     string
}

// WhatsappDeviceStatus probes the ONE global device: is wacli installed, is the host
// paired, and as which JID. Used by the CLI wizard so `channel add whatsapp` /
// `channel connect` never re-pair an already-linked device.
func WhatsappDeviceStatus(ctx context.Context, bin string) DeviceStatus {
	if bin == "" {
		bin = "wacli"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return DeviceStatus{}
	}
	out, err := exec.CommandContext(ctx, bin, "doctor", "--json").Output()
	if err != nil {
		return DeviceStatus{Installed: true}
	}
	var d struct {
		Data struct {
			Authenticated bool   `json:"authenticated"`
			LinkedJID     string `json:"linked_jid"`
		} `json:"data"`
	}
	if json.Unmarshal(out, &d) != nil {
		return DeviceStatus{Installed: true}
	}
	return DeviceStatus{Installed: true, Authenticated: d.Data.Authenticated, LinkedJID: d.Data.LinkedJID}
}
