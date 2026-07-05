package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/home"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// telegramChannel is one Telegram bot: inbound via the Bot API webhook (Telegram POSTs
// Updates to /telegram/<name>), outbound via sendMessage. Every telegram channel is its
// own bot — own token by NAME, own default chat id. Telegram optionally signs the
// webhook with a secret_token header; when options["secretHeader"] is configured it is
// verified against the resolved token.
type telegramChannel struct {
	name string
	cfg  config.Transport
	res  *SecretResolver
}

func openTelegram(name string, cfg config.Transport, res *SecretResolver) (Channel, error) {
	return &telegramChannel{name: name, cfg: cfg, res: res}, nil
}

func (t *telegramChannel) Name() string { return t.name }
func (t *telegramChannel) Kind() string { return "telegram" }

func (t *telegramChannel) Path() string {
	if p := t.cfg.Options["path"]; p != "" {
		return p
	}
	return "/telegram/" + t.name
}

// baseURL is the Bot API root — options["baseURL"] lets tests point every call
// (webhook download AND send) at a local server.
func (t *telegramChannel) baseURL() string {
	if b := t.cfg.Options["baseURL"]; b != "" {
		return b
	}
	return "https://api.telegram.org"
}

// tgFile is the common Telegram file descriptor (document / video / voice / audio).
type tgFile struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIMEType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// tgUpdate is the slice of the Telegram Update we normalize. MessageID is captured so a
// reply can thread via reply_to_message_id; caption carries the text of media messages.
// photo is an array of sizes ordered smallest→largest — we take the LAST (largest).
type tgUpdate struct {
	Message struct {
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		Caption   string `json:"caption"`
		From      struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Photo []struct {
			FileID   string `json:"file_id"`
			FileSize int64  `json:"file_size"`
		} `json:"photo"`
		Document *tgFile `json:"document"`
		Video    *tgFile `json:"video"`
		Voice    *tgFile `json:"voice"`
		Audio    *tgFile `json:"audio"`
	} `json:"message"`
}

// attachment normalizes the update's media (if any) to a metadata-only Attachment plus
// the telegram file_id to download. Empty file_id = no media on this message.
func (u tgUpdate) attachment() (envelope.Attachment, string) {
	m := u.Message
	switch {
	case len(m.Photo) > 0:
		p := m.Photo[len(m.Photo)-1] // last = largest
		return envelope.Attachment{Type: "image", Size: p.FileSize}, p.FileID
	case m.Document != nil:
		return envelope.Attachment{Type: "document", Name: m.Document.FileName, MIME: m.Document.MIMEType, Size: m.Document.FileSize}, m.Document.FileID
	case m.Video != nil:
		return envelope.Attachment{Type: "video", MIME: m.Video.MIMEType, Size: m.Video.FileSize}, m.Video.FileID
	case m.Voice != nil:
		return envelope.Attachment{Type: "voice", MIME: m.Voice.MIMEType, Size: m.Voice.FileSize}, m.Voice.FileID
	case m.Audio != nil:
		return envelope.Attachment{Type: "audio", Name: m.Audio.FileName, MIME: m.Audio.MIMEType, Size: m.Audio.FileSize}, m.Audio.FileID
	}
	return envelope.Attachment{}, ""
}

func (t *telegramChannel) Handler(pub Publisher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if hd := t.cfg.Options["secretHeader"]; hd != "" {
			want, err := t.res.Token(t.cfg)
			if err != nil || r.Header.Get(hd) != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		var u tgUpdate
		if err := json.Unmarshal(body, &u); err != nil {
			http.Error(w, "bad update", http.StatusBadRequest)
			return
		}
		sender := u.Message.From.Username
		if sender == "" {
			sender = strconv.FormatInt(u.Message.From.ID, 10)
		}
		// Media messages carry their text in caption.
		text := u.Message.Text
		if text == "" {
			text = u.Message.Caption
		}
		env := envelope.Inbound(t.name, sender, text, "Telegram")
		env.Account = t.cfg.Account
		// ID = telegram message_id so a reply can thread to this exact message.
		if u.Message.MessageID != 0 {
			env.ID = strconv.FormatInt(u.Message.MessageID, 10)
		}
		// ThreadID = chat id so a reply lands in the same chat.
		env.ThreadID = strconv.FormatInt(u.Message.Chat.ID, 10)
		if att, fileID := u.attachment(); fileID != "" {
			// Best-effort download: on ANY failure the envelope still ships with
			// the metadata-only attachment — a message is never dropped.
			if path, size, err := t.download(r.Context(), fileID, env.ID, att.Name); err == nil {
				att.Path = path
				att.Size = size
			}
			env.Attachments = append(env.Attachments, att)
		}
		pub(env)
		w.WriteHeader(http.StatusOK)
	})
}

// sanitizeFilename picks a safe basename for a downloaded file: the provider filename,
// else the Bot API file_path's base, else the raw file_id. filepath.Base strips any
// directory components a hostile payload could smuggle in.
func sanitizeFilename(name, filePath, fileID string) string {
	for _, cand := range []string{name, filePath} {
		if b := filepath.Base(cand); b != "" && b != "." && b != string(filepath.Separator) {
			return b
		}
	}
	return fileID
}

// download fetches one telegram file via getFile + the file endpoint and stores it
// under home.MediaDir() as "<messageID>-<name>". The token is resolved by NAME at the
// point of use and consumed only inside the two request URLs — never logged or stored.
func (t *telegramChannel) download(ctx context.Context, fileID, messageID, name string) (string, int64, error) {
	token, err := t.res.Token(t.cfg)
	if err != nil {
		return "", 0, err
	}
	base := t.baseURL()
	form := url.Values{"file_id": {fileID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/getFile", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("channel: telegram getFile: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", 0, fmt.Errorf("channel: telegram getFile: status %d", resp.StatusCode)
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil || !out.OK || out.Result.FilePath == "" {
		return "", 0, fmt.Errorf("channel: telegram getFile: bad response")
	}
	freq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/file/bot"+token+"/"+out.Result.FilePath, nil)
	if err != nil {
		return "", 0, err
	}
	fresp, err := httpClient.Do(freq)
	if err != nil {
		return "", 0, fmt.Errorf("channel: telegram file fetch: %w", err)
	}
	defer fresp.Body.Close()
	if fresp.StatusCode/100 != 2 {
		return "", 0, fmt.Errorf("channel: telegram file fetch: status %d", fresp.StatusCode)
	}
	dir := home.MediaDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, fmt.Errorf("channel: telegram media dir: %w", err)
	}
	dest := filepath.Join(dir, messageID+"-"+sanitizeFilename(name, out.Result.FilePath, fileID))
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("channel: telegram media save: %w", err)
	}
	n, err := io.Copy(f, fresp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, fmt.Errorf("channel: telegram media save: %w", err)
	}
	return dest, n, nil
}

// telegramMethod maps an attachment type to its Bot API send method and the name of
// the media field that carries the file (or URL).
func telegramMethod(typ string) (method, field string) {
	switch typ {
	case "image":
		return "sendPhoto", "photo"
	case "video":
		return "sendVideo", "video"
	case "voice":
		return "sendVoice", "voice"
	case "audio":
		return "sendAudio", "audio"
	default:
		return "sendDocument", "document"
	}
}

// Send delivers via sendMessage (text) or the type-matched media method (attachments),
// threading with reply_to_message_id when ReplyTo is set, and returns the message_id
// Telegram assigned so the caller can thread onto its own outbound message. With
// attachments, the FIRST carries env.Text as its caption and its message_id is the one
// returned; additional attachments are sent the same way without a caption.
func (t *telegramChannel) Send(ctx context.Context, env envelope.Envelope) (string, error) {
	token, err := t.res.Token(t.cfg)
	if err != nil {
		return "", err
	}
	base := t.baseURL()
	to := env.ThreadID
	if to == "" {
		to = t.cfg.Options["chatId"]
	}
	if to == "" {
		return "", fmt.Errorf("channel: telegram %q: no target (pass --to or configure --chat-id)", t.name)
	}
	if len(env.Attachments) == 0 {
		form := url.Values{"chat_id": {to}, "text": {env.Text}}
		if env.ReplyTo != "" {
			form.Set("reply_to_message_id", env.ReplyTo)
		}
		// The token is a path segment, consumed only here — never logged.
		return t.postAndID(ctx, base+"/bot"+token+"/sendMessage", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	}
	firstID := ""
	for i, att := range env.Attachments {
		caption := ""
		if i == 0 {
			caption = env.Text
		}
		id, err := t.sendAttachment(ctx, base, token, to, env.ReplyTo, att, caption)
		if err != nil {
			return "", err
		}
		if i == 0 {
			firstID = id
		}
	}
	return firstID, nil
}

// sendAttachment delivers one attachment via its Bot API method. A local Path is
// uploaded multipart; a URL-only attachment is passed as a plain form string (Telegram
// fetches it server-side). token is consumed only inside the endpoint — never logged.
func (t *telegramChannel) sendAttachment(ctx context.Context, base, token, to, replyTo string, att envelope.Attachment, caption string) (string, error) {
	method, field := telegramMethod(att.Type)
	endpoint := base + "/bot" + token + "/" + method
	if att.Path == "" {
		if att.URL == "" {
			return "", fmt.Errorf("channel: telegram %q: attachment needs a path or url", t.name)
		}
		form := url.Values{"chat_id": {to}, field: {att.URL}}
		if caption != "" {
			form.Set("caption", caption)
		}
		if replyTo != "" {
			form.Set("reply_to_message_id", replyTo)
		}
		return t.postAndID(ctx, endpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	}
	f, err := os.Open(att.Path)
	if err != nil {
		return "", fmt.Errorf("channel: telegram %q: attachment: %w", t.name, err)
	}
	defer f.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("chat_id", to); err != nil {
		return "", err
	}
	if caption != "" {
		if err := mw.WriteField("caption", caption); err != nil {
			return "", err
		}
	}
	if replyTo != "" {
		if err := mw.WriteField("reply_to_message_id", replyTo); err != nil {
			return "", err
		}
	}
	name := att.Name
	if name == "" {
		name = filepath.Base(att.Path)
	}
	part, err := mw.CreateFormFile(field, name)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("channel: telegram %q: attachment read: %w", t.name, err)
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	return t.postAndID(ctx, endpoint, mw.FormDataContentType(), &buf)
}

// postAndID POSTs body to a Bot API endpoint and returns result.message_id from the
// response. The endpoint embeds the token as a path segment; errors carry only the
// HTTP status — never the URL.
func (t *telegramChannel) postAndID(ctx context.Context, endpoint, contentType string, body io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("channel: telegram deliver: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("channel: telegram deliver: status %d", resp.StatusCode)
	}
	var out struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &out) == nil && out.Result.MessageID != 0 {
		return strconv.FormatInt(out.Result.MessageID, 10), nil
	}
	return "", nil
}
