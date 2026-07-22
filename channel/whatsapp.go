package channel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/home"
)

// WhatsApp is GLOBAL: the host has ONE paired device (wacli's linked WhatsApp-Web
// session), and every configured whatsapp channel is a named GROUP on that device.
// Exactly one `wacli sync --follow --webhook …` subprocess runs no matter how many
// whatsapp channels exist; inbound is routed STRICTLY to the channel whose
// options["group"] JID matches the message's chat — a chat with no bound channel is
// dropped, never mixed into another conversation (no catch-all). Sends target the
// channel's group (or an
// explicit --to thread), threading via wacli --reply-to when ReplyTo is set.

// maxAttachmentFetch caps how much of a remote attachment URL we pull to a temp file
// before handing it to wacli (100 MB — beyond WhatsApp's own media ceiling anyway).
const maxAttachmentFetch = 100 << 20

// whatsappKind is the whole kind: wire behavior (Open + the ONE shared stream) and CLI
// behavior (device wizardry, group rules) together.
type whatsappKind struct{ Base }

func init() { Register(whatsappKind{}) }

func (whatsappKind) Name() string   { return "whatsapp" }
func (whatsappKind) Traits() Traits { return Traits{TargetFlag: "group"} }

func (whatsappKind) Open(name string, cfg config.Transport, res *SecretResolver) (Channel, error) {
	return openWhatsapp(name, cfg, res)
}

func (whatsappKind) OpenStream(chans map[string]config.Transport, res *SecretResolver) (Streamer, error) {
	return openWhatsappStream(chans, res)
}

func (whatsappKind) Test(ctx context.Context, name string, cfg config.Transport, res *SecretResolver) ([]string, error) {
	return testWhatsapp(ctx, name, cfg, res)
}

// Validate: one group JID = ONE channel — a duplicate bind silently shadows the first
// (first match wins in stream routing), so refuse it at add time.
func (whatsappKind) Validate(name string, cfg config.Transport, existing map[string]config.Transport) error {
	g := cfg.Options["group"]
	if g == "" {
		return nil
	}
	for other, t := range existing {
		if other != name && t.Options["group"] == g {
			return fmt.Errorf("group %s is already bound to channel %q — one JID = one channel", g, other)
		}
	}
	return nil
}

// AddHints: the device is GLOBAL — say whether pairing is even needed, and nudge
// toward a group binding when none was given.
func (whatsappKind) AddHints(name string, cfg config.Transport) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var hints []string
	st := WhatsappDeviceStatus(ctx, waBin(cfg))
	switch {
	case !st.Installed:
		hints = append(hints, "wacli not found on PATH — install it, then: messenger channel connect "+name)
	case st.Authenticated:
		hints = append(hints, fmt.Sprintf("device already linked (%s) — no pairing needed", st.LinkedJID))
	default:
		hints = append(hints, fmt.Sprintf("pair the device once (serves ALL whatsapp channels): messenger channel connect %s", name))
	}
	if cfg.Options["group"] == "" {
		hints = append(hints,
			"no --group set: this channel is SEND-ONLY and receives NOTHING (no catch-all).",
			fmt.Sprintf("  bind a group to receive: messenger channel connect %s   # lists your FREE groups + their JIDs", name))
	}
	return hints
}

// Connect is the wizard for the ONE global device: already linked → report + list the
// FREE groups (JIDs not yet bound to a channel); not linked → run the interactive QR
// pair, once for all whatsapp channels.
func (whatsappKind) Connect(name string, cfg config.Transport, p ConnectParams) error {
	bin := waBin(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := WhatsappDeviceStatus(ctx, bin)
	if !st.Installed {
		return fmt.Errorf("wacli not found on PATH — install it first (https://wacli.sh)")
	}
	if !st.Authenticated {
		fmt.Printf("pairing the global whatsapp device via %s auth — scan the QR (once per host):\n", bin)
		cmd := exec.Command(bin, "auth")
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		return cmd.Run()
	}
	fmt.Printf("whatsapp device already linked (%s) — serves every whatsapp channel, no re-pair needed.\n", st.LinkedJID)
	free, bound := freeGroups(ctx, bin, p.Existing)
	switch {
	case len(free) == 0 && bound == 0:
		fmt.Println("no groups in the local store yet — run `wacli sync` once, then re-run connect.")
	case len(free) == 0:
		fmt.Printf("no free groups: all %d known group(s) are already bound to channels.\n", bound)
	default:
		fmt.Printf("free groups (%d already bound are hidden) — bind one with: messenger register <agent> --group <jid>\n", bound)
		for _, g := range free {
			fmt.Printf("  %-40s %s\n", g.Name, g.JID)
		}
	}
	return nil
}

func (whatsappKind) Detail(name string, cfg config.Transport) string {
	if g := cfg.Options["group"]; g != "" {
		return "group=" + g
	}
	return "(no group — send-only, receives nothing)"
}

// Lane: an agent's whatsapp lane is a channel bound to its group. A groupless lane is
// send-only (no catch-all) — still allowed for outbound-to-any-JID use.
func (k whatsappKind) Lane(name string, p LaneParams, existing map[string]config.Transport) (config.Transport, []string, error) {
	var opts map[string]string
	if p.Group != "" {
		opts = map[string]string{"group": p.Group}
	}
	want := config.Transport{Enabled: true, Kind: "whatsapp", Options: opts}
	if err := k.Validate(name, want, existing); err != nil {
		return config.Transport{}, nil, fmt.Errorf("%w (subscribe to the existing channel instead: --channels <its name>)", err)
	}
	hint := fmt.Sprintf("channel %q → whatsapp group %s", name, p.Group)
	if p.Group == "" {
		hint = fmt.Sprintf("channel %q → whatsapp send-only (no --group; receives nothing)", name)
	}
	return want, []string{hint}, nil
}

// Status: the GLOBAL device state, shown by setup/status so the user knows whether
// pairing is even needed.
func (whatsappKind) Status() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := WhatsappDeviceStatus(ctx, "")
	switch {
	case !st.Installed:
		return []string{"whatsapp: wacli not found on PATH — install it to use whatsapp channels"}
	case st.Authenticated:
		return []string{fmt.Sprintf("whatsapp: device already linked (%s) — no pairing needed, just add group channels", st.LinkedJID)}
	default:
		return []string{"whatsapp: wacli installed but not paired — `messenger channel connect <name>` will run the QR pair once"}
	}
}

// waGroup is one group from the local wacli store.
type waGroup struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

// freeGroups lists the device's known groups MINUS the ones already bound to a channel,
// plus how many were hidden — so the wizard only offers groups that can still be bound.
func freeGroups(ctx context.Context, bin string, existing map[string]config.Transport) ([]waGroup, int) {
	out, err := exec.CommandContext(ctx, bin, "--json", "groups", "list").Output()
	if err != nil {
		return nil, 0
	}
	var res struct {
		Data []waGroup `json:"data"`
	}
	if json.Unmarshal(out, &res) != nil {
		return nil, 0
	}
	taken := map[string]bool{}
	for _, t := range existing {
		if g := t.Options["group"]; g != "" {
			taken[g] = true
		}
	}
	var free []waGroup
	bound := 0
	for _, g := range res.Data {
		if taken[g.JID] {
			bound++
			continue
		}
		free = append(free, g)
	}
	return free, bound
}

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

// sendDispatchTimeout bounds one wacli send invocation, DETACHED from the caller's own
// context (see dispatchContext). wacli's own attempt budget is 45s (sendAttemptTimeout in
// wacli/cmd/wacli/send_helpers.go) and a retryable failure reconnects and tries a SECOND
// 45s attempt — plus ffprobe/ffmpeg voice-note metadata probing and the media upload
// itself for a large attachment. 150s gives that realistic worst case headroom without
// being unboundedly long.
const sendDispatchTimeout = 150 * time.Second

// dispatchContext detaches a wacli send from the CALLER's cancellation (an impatient CLI
// wrapper's own short deadline, an HTTP client that walked away) while still bounding it
// with our own timeout. This is the delivered-send-never-retries invariant: once we shell
// out to wacli, the message MAY already be in flight to WhatsApp's servers by the time it
// would ack — killing that subprocess early doesn't undo the send, it just makes messenger
// falsely report a failure, and a caller that reacts to a false failure by retrying
// produces a genuine duplicate delivery. Letting the dispatch run to its OWN true
// completion (success or real failure) means the result messenger reports is always
// accurate, so there is never a false failure to retry.
func dispatchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), sendDispatchTimeout)
}

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
	sendCtx, cancel := dispatchContext(ctx)
	defer cancel()
	out, err := c.sendWithReplyFallback(sendCtx, waBin(c.cfg), args, env.ReplyTo, env.Sender)
	if err != nil {
		return "", fmt.Errorf("channel: whatsapp: wacli send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseWacliSendID(out), nil
}

// sendWithReplyFallback runs a wacli send and, when a GROUP reply fails because wacli's sync
// store doesn't have the target message ("--reply-to-sender is required for unsynced group
// replies"), retries ONCE adding --reply-to-sender <sender>. The inbound envelope already
// carries the sender JID, so threaded group replies stay reliable even when the message
// isn't in wacli's local store — the exact flakiness where the same reply worked once then
// 502'd. A non-reply send, or one with no sender, is passed through unchanged.
func (c *whatsappChannel) sendWithReplyFallback(ctx context.Context, bin string, args []string, replyTo, sender string) ([]byte, error) {
	out, err := c.runCmd(ctx, bin, args...)
	if err != nil && replyTo != "" && sender != "" && strings.Contains(string(out), "reply-to-sender") {
		// Full-slice cap so the retry doesn't clobber the caller's args backing array.
		retry := append(args[:len(args):len(args)], "--reply-to-sender", sender)
		out, err = c.runCmd(ctx, bin, retry...)
	}
	return out, err
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
		if att.Type == "voice" {
			// wacli renders OGG/Opus as a proper PTT voice-note bubble only with --ptt;
			// without it the same file lands as a generic file attachment.
			args = append(args, "--ptt")
		}
		if i == 0 && env.Text != "" {
			args = append(args, "--caption", env.Text)
		}
		if env.ReplyTo != "" {
			args = append(args, "--reply-to", env.ReplyTo)
		}
		sendCtx, cancel := dispatchContext(ctx)
		out, err := c.sendWithReplyFallback(sendCtx, bin, args, env.ReplyTo, env.Sender)
		cancel()
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

// whatsappStream is the ONE shared inbound stream for every whatsapp channel.
//
// wacli delivers inbound over an HTTP WEBHOOK, NOT stdout: `wacli sync --follow` only
// holds the WhatsApp connection open; `--webhook <URL>` is the sole channel for live
// messages (it POSTs each message's JSON there). So the stream both (a) runs the
// long-lived `wacli sync --follow --webhook <hub>/_wacli/inbound --webhook-allow-private
// --webhook-secret <s>` subprocess, supervised by the runtime, AND (b) mounts an HTTP
// handler at that path that verifies the X-Wacli-Signature HMAC and publishes each
// posted message. Routing is strict: a message is delivered ONLY to the channel bound
// to its chat JID — there is NO catch-all, so an unbound group is dropped (never mixed
// into another conversation). Our own outbound echoes (from_me) are dropped too.
type whatsappStream struct {
	bin  string
	args []string // override (tests); empty = build the webhook argv in Run

	byGroup  map[string]string // group jid -> channel name (the ONLY inbound routes)
	accounts map[string]string // channel name -> account label

	callbackURL string // hub loopback URL wacli POSTs to (injected by the runtime)
	secret      string // per-boot HMAC secret shared with wacli --webhook-secret

	// commandContext is the exec seam so tests inject a fake subprocess.
	commandContext func(ctx context.Context, name string, arg ...string) *exec.Cmd
	// runCmd is the exec seam for one-shot wacli calls (media download).
	runCmd runCmdFunc
}

// wacliInboundPath is where the runtime mounts the receiver and where wacli POSTs.
const wacliInboundPath = "/_wacli/inbound"

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
		secret:         newInboundSecret(),
		commandContext: exec.CommandContext,
		runCmd:         defaultRunCmd,
	}
	for _, n := range names {
		cfg := chans[n]
		// Only group-bound channels receive. A no-group channel is send-only (it never
		// catches unmatched chats — one group, one channel).
		if g := cfg.Options["group"]; g != "" {
			s.byGroup[g] = n
		}
		s.accounts[n] = cfg.Account
		if s.bin == "" {
			s.bin = cfg.Options["bin"]
		}
		if len(s.args) == 0 && cfg.Options["args"] != "" {
			s.args = strings.Fields(cfg.Options["args"])
		}
	}
	if s.bin == "" {
		s.bin = "wacli"
	}
	return s, nil
}

// newInboundSecret is the HMAC secret for the wacli→hub webhook. It is pinnable via
// MESSENGER_WA_INBOUND_SECRET (handy for debugging or a fixed multi-process setup);
// otherwise a per-boot random one is minted. It is handed to the subprocess and used to
// verify posts — never written to user config. "" (rand failure) = accept unsigned
// loopback posts.
func newInboundSecret() string {
	if s := os.Getenv("MESSENGER_WA_INBOUND_SECRET"); s != "" {
		return s
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// Path/Handler/UseCallback make the stream a WebhookInbound the runtime mounts.
func (s *whatsappStream) Path() string         { return wacliInboundPath }
func (s *whatsappStream) UseCallback(u string) { s.callbackURL = u }

func (s *whatsappStream) Handler(pub Publisher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		// The hub is publicly exposed (telegram needs it), so this endpoint MUST be
		// authenticated: wacli signs with the shared secret (sha256=<hex>).
		if s.secret != "" && !VerifyHMAC([]byte(s.secret), body, r.Header.Get("X-Wacli-Signature")) {
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
			return
		}
		s.ingest(r.Context(), body, pub)
		w.WriteHeader(http.StatusOK)
	})
}

// waMessage is the slice of a wacli message JSON we normalize. Fields arrive in either
// casing depending on the wacli surface (compact lowercase over the webhook, PascalCase
// from the local store), so both spellings are parsed and the accessors pick whichever
// is set.
type waMessage struct {
	// id: compact webhook (id/msg_id/message_id) or store (MsgID).
	ID        string `json:"id"`
	MsgID     string `json:"msg_id"`
	MessageID string `json:"message_id"`
	MsgIDPC   string `json:"MsgID"`
	// chat: chat/chat_jid (compact) or ChatJID (store).
	Chat    string `json:"chat"`
	ChatJID string `json:"chat_jid"`
	ChatPC  string `json:"ChatJID"`
	// sender: sender/sender_jid (compact) or SenderJID/SenderName (store).
	Sender     string `json:"sender"`
	SenderJID  string `json:"sender_jid"`
	SenderPC   string `json:"SenderJID"`
	SenderName string `json:"SenderName"`
	// text: text/body (compact) or Text (store).
	Text   string `json:"text"`
	Body   string `json:"body"`
	TextPC string `json:"Text"`

	FromMeLC bool `json:"from_me"`
	FromMeCC bool `json:"fromMe"`
	FromMePC bool `json:"FromMe"`

	MediaTypeLC string `json:"media_type"`
	MediaTypePC string `json:"MediaType"`
	CaptionLC   string `json:"caption"`
	CaptionPC   string `json:"MediaCaption"`
	FilenameLC  string `json:"filename"`
	FilenamePC  string `json:"Filename"`
	MIMELC      string `json:"mime"`
	MIMEPC      string `json:"MimeType"`

	// Media is the LIVE wacli sync --webhook shape: media metadata is NOT a flat
	// media_type on the message, it is a nested object (`"Media":{...}` — null for a
	// plain text message). The flat fields above are the store/compact spellings; this
	// covers the webhook that actually feeds the hub. Kept as raw JSON so a null or an
	// unexpected inner shape never breaks parsing.
	Media json.RawMessage `json:"media"`
}

// waMedia is the nested Media object wacli posts over the webhook for an attachment.
// Field spellings are matched case-insensitively by encoding/json, so "MimeType",
// "mimetype" and "Mimetype" all land here.
type waMedia struct {
	Kind      string `json:"kind"`
	Type      string `json:"type"`
	MediaType string `json:"mediatype"`
	Mimetype  string `json:"mimetype"`
	Mime      string `json:"mime"`
	Name      string `json:"name"`
	File      string `json:"file"`
	Filename  string `json:"filename"`
	Caption   string `json:"caption"`
}

// media returns the parsed nested Media object and whether the message carries one. A
// missing, null, or unparseable Media is (nil,false) — the message is treated as
// text-only.
func (m waMessage) media() (*waMedia, bool) {
	trimmed := bytes.TrimSpace(m.Media)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false
	}
	var md waMedia
	if json.Unmarshal(trimmed, &md) != nil {
		// Present but not the object shape we know — still an attachment; download by id.
		return &waMedia{}, true
	}
	return &md, true
}

func (m waMessage) id() string {
	return firstNonEmpty(firstNonEmpty(m.ID, m.MsgID), firstNonEmpty(m.MessageID, m.MsgIDPC))
}
func (m waMessage) chat() string { return firstNonEmpty(firstNonEmpty(m.Chat, m.ChatJID), m.ChatPC) }
func (m waMessage) sender() string {
	return firstNonEmpty(firstNonEmpty(m.Sender, m.SenderJID), firstNonEmpty(m.SenderPC, m.SenderName))
}
func (m waMessage) text() string { return firstNonEmpty(firstNonEmpty(m.Text, m.Body), m.TextPC) }
func (m waMessage) fromMe() bool { return m.FromMeLC || m.FromMeCC || m.FromMePC }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// mediaType reports the attachment type. It prefers the flat store/compact fields, then
// the nested webhook Media object. A message that carries a Media object but no
// recognizable type string still reports "file" so it is ALWAYS treated as an
// attachment (and downloaded) — a voice note must never be dropped to text-only.
func (m waMessage) mediaType() string {
	if t := firstNonEmpty(m.MediaTypeLC, m.MediaTypePC); t != "" {
		return t
	}
	if md, ok := m.media(); ok {
		if t := firstNonEmpty(firstNonEmpty(md.Kind, md.Type), md.MediaType); t != "" {
			return t
		}
		return "file" // has an attachment, type unknown — never drop it
	}
	return ""
}
func (m waMessage) caption() string {
	if c := firstNonEmpty(m.CaptionLC, m.CaptionPC); c != "" {
		return c
	}
	if md, ok := m.media(); ok {
		return md.Caption
	}
	return ""
}
func (m waMessage) filename() string {
	if f := firstNonEmpty(m.FilenameLC, m.FilenamePC); f != "" {
		return f
	}
	if md, ok := m.media(); ok {
		return firstNonEmpty(md.Name, md.File)
	}
	return ""
}
func (m waMessage) mime() string {
	if mm := firstNonEmpty(m.MIMELC, m.MIMEPC); mm != "" {
		return mm
	}
	if md, ok := m.media(); ok {
		return firstNonEmpty(md.Mimetype, md.Mime)
	}
	return ""
}

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

// route returns the channel bound to chat, or "" when no channel owns it. There is NO
// catch-all: an unrouted message is dropped, never delivered to another channel.
func (s *whatsappStream) route(chat string) string { return s.byGroup[chat] }

// Run launches the long-lived `wacli sync --follow --webhook …` subprocess that holds
// the connection and POSTs inbound to our handler. stdout carries lifecycle noise
// (messages arrive over the webhook) and is drained — EXCEPT we watch for one fatal
// case: wacli exiting immediately because the store is already locked by ANOTHER wacli
// (`{"event":"error","data":{"message":"store is locked ... pid=N"}}`). The device is
// global and single-hub by design — exactly one wacli sync may hold the store — so any
// other process holding it is a stray (the classic dual-instance "device theft": a
// leftover legacy listener grabs the lock and starves the hub, and the supervisor then
// blind-loops the exit-1 forever). When we see that, we REAP the strayed holder (only
// if it is really a wacli process and not us) and retry the sync ONCE inline, so the
// stream self-heals instead of looping. The runtime still supervises the outer error.
func (s *whatsappStream) Run(ctx context.Context, _ Publisher) error {
	args := s.args
	if len(args) == 0 {
		if s.callbackURL == "" {
			return fmt.Errorf("channel: whatsapp: no inbound webhook URL — the hub self URL is unknown (serve/listen must set it)")
		}
		args = []string{"--json", "sync", "--follow", "--webhook", s.callbackURL, "--webhook-allow-private"}
		if s.secret != "" {
			args = append(args, "--webhook-secret", s.secret)
		}
	}
	waitErr, lockPID := s.runSync(ctx, args)
	if ctx.Err() != nil {
		return nil // cancelled: clean stop
	}
	if waitErr == nil {
		return nil
	}
	// Store locked by a stray wacli: reap it (never ourselves) and retry once inline.
	if lockPID > 0 && reapStrayWacli(lockPID) {
		fmt.Printf("messenger: whatsapp reaped stray wacli holding the store (pid=%d), retrying sync\n", lockPID)
		waitErr, _ = s.runSync(ctx, args)
		if ctx.Err() != nil {
			return nil
		}
		if waitErr == nil {
			return nil
		}
	}
	return fmt.Errorf("channel: whatsapp: wacli exited: %w", waitErr)
}

// storeLockedRe pulls the offending PID out of wacli's store-locked error line.
var storeLockedRe = regexp.MustCompile(`store is locked[^}]*pid=(\d+)`)

// runSync runs one wacli sync subprocess to completion, draining stdout while sniffing
// it for a store-locked error. It returns the subprocess exit error and, if the exit was
// caused by another wacli holding the store, that holder's PID (else 0).
func (s *whatsappStream) runSync(ctx context.Context, args []string) (error, int) {
	cmd := s.commandContext(ctx, s.bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err, 0
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("channel: whatsapp: start %q: %w", s.bin, err), 0
	}
	lockPID := make(chan int, 1)
	go func() {
		found := 0
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if found == 0 {
				if mm := storeLockedRe.FindSubmatch(sc.Bytes()); mm != nil {
					if pid, e := strconv.Atoi(string(mm[1])); e == nil {
						found = pid
					}
				}
			}
		}
		lockPID <- found
	}()
	waitErr := cmd.Wait()
	return waitErr, <-lockPID
}

// reapStrayWacli kills the process holding wacli's store lock — but ONLY when it is
// genuinely a wacli process and not this hub's own process tree. Returns true if it
// reaped something. This enforces the single-hub invariant: one device, one wacli.
func reapStrayWacli(pid int) bool {
	if pid <= 0 || pid == os.Getpid() {
		return false
	}
	// Verify it's actually a wacli process before killing anything.
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || !strings.Contains(strings.ToLower(string(out)), "wacli") {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Kill(); err != nil {
		return false
	}
	// Give the OS a moment to release the flock before the caller retries.
	time.Sleep(500 * time.Millisecond)
	return true
}

// ingest parses a wacli webhook body — a single message object, a `{message|data:{…}}`
// wrapper, or a `{messages:[…]}` / bare `[…]` array — and publishes each routed message.
func (s *whatsappStream) ingest(ctx context.Context, body []byte, pub Publisher) {
	for _, raw := range messageItems(body) {
		s.publishMessage(ctx, raw, pub)
	}
}

// messageItems teases the individual message JSONs out of whatever envelope wacli used.
func messageItems(body []byte) []json.RawMessage {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(trimmed, &arr) == nil {
			return arr
		}
	}
	var wrap struct {
		Messages []json.RawMessage `json:"messages"`
		Message  json.RawMessage   `json:"message"`
		Data     json.RawMessage   `json:"data"`
	}
	if json.Unmarshal(trimmed, &wrap) == nil {
		switch {
		case len(wrap.Messages) > 0:
			return wrap.Messages
		case len(wrap.Message) > 0:
			return []json.RawMessage{wrap.Message}
		case len(wrap.Data) > 0:
			var arr []json.RawMessage
			if json.Unmarshal(wrap.Data, &arr) == nil {
				return arr
			}
			return []json.RawMessage{wrap.Data}
		}
	}
	return []json.RawMessage{trimmed} // treat the whole body as one message
}

// publishMessage normalizes ONE message and publishes it when it carries content and is
// bound to a channel. Our own echoes (from_me) and messages for unbound chats are
// dropped. Media is downloaded best-effort; a failed download still publishes the
// envelope with a metadata-only attachment — an inbound message is never lost to a fetch
// error.
func (s *whatsappStream) publishMessage(ctx context.Context, raw json.RawMessage, pub Publisher) {
	var m waMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	if m.fromMe() {
		return // our own outbound echoed back — not inbound
	}
	if m.text() == "" && m.mediaType() == "" {
		return
	}
	name := s.route(m.chat())
	if name == "" {
		return // no channel bound to this chat — dropped (no catch-all)
	}
	text := m.text()
	if text == "" {
		text = m.caption()
	}
	env := envelope.Inbound(name, m.sender(), text, "WhatsApp")
	env.Account = s.accounts[name]
	if m.id() != "" {
		env.ID = m.id() // wacli's stable message id → reply/dedupe key
	}
	if m.chat() != "" {
		env.ThreadID = m.chat()
	}
	if mt := m.mediaType(); mt != "" {
		att := envelope.Attachment{Type: attachmentType(mt), Name: m.filename(), MIME: m.mime()}
		if p := s.downloadMedia(ctx, m.chat(), m.id()); p != "" {
			att.Path = p
			if fi, err := os.Stat(p); err == nil {
				att.Size = fi.Size()
			}
		}
		env.Attachments = []envelope.Attachment{att}
	}
	pub(env)
}

// downloadMedia shells `wacli --read-only media download` into home.MediaDir() and
// returns the stored file's path, or "" on any failure (the caller publishes
// regardless). --read-only (with an explicit --output) is required here: the hub's
// long-lived `wacli sync --follow` subprocess (Run, above) holds wacli's store lock for
// its entire lifetime, so a second, non-read-only `wacli media download` invocation
// always fails with "store is locked" (it tries to write local_path/downloaded_at back
// into wacli.db). --read-only fetches the media over the WhatsApp connection without
// touching the store at all, sidestepping the lock entirely — verified against a live
// wacli 0.11.1 store with sync --follow running.
func (s *whatsappStream) downloadMedia(ctx context.Context, chat, id string) string {
	if chat == "" || id == "" {
		return ""
	}
	dir := home.MediaDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	out, err := s.runCmd(ctx, s.bin, "--read-only", "media", "download", "--chat", chat, "--id", id, "--output", dir, "--json")
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
