package channel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// webhookKind is the whole kind: wire behavior + CLI behavior. Every hook is its own
// channel with its own path and HMAC secret (by NAME).
type webhookKind struct{ Base }

func init() { Register(webhookKind{}) }

func (webhookKind) Name() string   { return "webhook" }
func (webhookKind) Traits() Traits { return Traits{RequiresToken: true} }

func (webhookKind) Open(name string, cfg config.Transport, res *SecretResolver) (Channel, error) {
	return openWebhook(name, cfg, res)
}

func (webhookKind) Test(ctx context.Context, name string, cfg config.Transport, res *SecretResolver) ([]string, error) {
	return testWebhook(ctx, name, cfg, res)
}

func (webhookKind) AddHints(name string, cfg config.Transport) []string {
	return []string{fmt.Sprintf("inbound path: %s (HMAC X-Hub-Signature-256 with $%s)",
		webhookPath(name, cfg), envNameOr(cfg.TokenEnv, "MESSENGER_HOOK_SECRET"))}
}

// Connect prints a signed-call example — no pairing needed; the secret stays a NAME.
func (webhookKind) Connect(name string, cfg config.Transport, _ ConnectParams) error {
	p := webhookPath(name, cfg)
	secret := envNameOr(cfg.TokenEnv, "MESSENGER_HOOK_SECRET")
	fmt.Printf("webhook %q needs no pairing. callers POST to %s signed with $%s:\n", name, p, secret)
	fmt.Printf("  sig=\"sha256=$(printf '%%s' \"$BODY\" | openssl dgst -sha256 -hmac \"$%s\" -hex | awk '{print $NF}')\"\n", secret)
	fmt.Printf("  curl -sS -X POST \"http://<host>%s\" -H \"X-Hub-Signature-256: $sig\" -d \"$BODY\"\n", p)
	return nil
}

func (webhookKind) Detail(name string, cfg config.Transport) string {
	return "path=" + webhookPath(name, cfg)
}

// Lane: an agent's webhook lane is its own signed inject path.
func (webhookKind) Lane(name string, p LaneParams, _ map[string]config.Transport) (config.Transport, []string, error) {
	if p.TokenEnv == "" {
		return config.Transport{}, nil, fmt.Errorf("webhook lanes need a secret: pass --token-env NAME")
	}
	want := config.Transport{Enabled: true, Kind: "webhook", TokenEnv: p.TokenEnv}
	return want, []string{fmt.Sprintf("channel %q → webhook /webhook/%s (HMAC $%s)", name, name, p.TokenEnv)}, nil
}

// webhookPath is the channel's inbound mount (options["path"] override).
func webhookPath(name string, cfg config.Transport) string {
	if p := cfg.Options["path"]; p != "" {
		return p
	}
	return "/webhook/" + name
}

// webhookChannel is the generic HMAC-signed channel — every hook is its own channel
// with its own path and secret. Inbound: a caller POSTs a body plus an
// X-Hub-Signature-256 header (sha256=<hex>) computed over the body with the shared
// secret; a verified payload becomes an inbound Envelope, an unverified one is 401.
// Outbound: the envelope is POSTed to options["callbackURL"], signed the same way.
// The secret is resolved by NAME — its value never enters config, a log, or the envelope.
type webhookChannel struct {
	name string
	cfg  config.Transport
	res  *SecretResolver
}

func openWebhook(name string, cfg config.Transport, res *SecretResolver) (Channel, error) {
	return &webhookChannel{name: name, cfg: cfg, res: res}, nil
}

func (h *webhookChannel) Name() string { return h.name }
func (h *webhookChannel) Kind() string { return "webhook" }

// Path defaults to /webhook/<name>; overridable via options["path"]. The legacy
// /hook/<name> keeps working via the alias mount in the runtime.
func (h *webhookChannel) Path() string {
	if p := h.cfg.Options["path"]; p != "" {
		return p
	}
	return "/webhook/" + h.name
}

func (h *webhookChannel) signatureHeader() string {
	if hd := h.cfg.Options["signatureHeader"]; hd != "" {
		return hd
	}
	return "X-Hub-Signature-256"
}

func (h *webhookChannel) Handler(pub Publisher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		secret, err := h.res.Token(h.cfg)
		if err != nil {
			http.Error(w, "unconfigured", http.StatusInternalServerError)
			return
		}
		if !VerifyHMAC([]byte(secret), body, r.Header.Get(h.signatureHeader())) {
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
			return
		}
		pub(normalizeWebhook(h.name, h.cfg.Account, body))
		w.WriteHeader(http.StatusAccepted)
	})
}

// Send POSTs the envelope to the channel's callbackURL, HMAC-signed when a secret is
// configured. The envelope's minted id is the caller's thread key.
func (h *webhookChannel) Send(ctx context.Context, env envelope.Envelope) (string, error) {
	callback := h.cfg.Options["callbackURL"]
	if callback == "" {
		return "", fmt.Errorf("channel: webhook %q: no callbackURL", h.name)
	}
	body, _ := json.Marshal(env)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callback, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.cfg.TokenEnv != "" || h.cfg.TokenVault != "" {
		secret, serr := h.res.Token(h.cfg)
		if serr != nil {
			return "", serr
		}
		req.Header.Set("X-Hub-Signature-256", SignHMAC([]byte(secret), body))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("channel: webhook deliver: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("channel: webhook deliver: status %d", resp.StatusCode)
	}
	return env.ID, nil
}

// SignHMAC returns the sha256=<hex> signature of body under secret.
func SignHMAC(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// VerifyHMAC is the constant-time check.
func VerifyHMAC(secret, body []byte, sig string) bool {
	if sig == "" {
		return false
	}
	return hmac.Equal([]byte(SignHMAC(secret, body)), []byte(sig))
}

// normalizeWebhook turns a verified payload into the canonical inbound Envelope. It
// reads a best-effort {text|message, sender|login, thread_id, reply_to, id, attachments}
// shape and falls back to the raw body as text, so any signed caller can inject a
// message. Attachments pass through by reference (type/name/mime/url/size); a remote
// caller's `path` is never trusted — Path names local files only, minted by messenger.
func normalizeWebhook(name, account string, body []byte) envelope.Envelope {
	var p struct {
		Text        string `json:"text"`
		Message     string `json:"message"`
		Sender      string `json:"sender"`
		Login       string `json:"login"`
		ThreadID    string `json:"thread_id"`
		ReplyTo     string `json:"reply_to"`
		ID          string `json:"id"`
		Attachments []struct {
			Type string `json:"type"`
			Name string `json:"name"`
			MIME string `json:"mime"`
			URL  string `json:"url"`
			Size int64  `json:"size"`
		} `json:"attachments"`
	}
	_ = json.Unmarshal(body, &p)
	text := p.Text
	if text == "" {
		text = p.Message
	}
	if text == "" {
		text = string(body)
	}
	sender := p.Sender
	if sender == "" {
		sender = p.Login
	}
	env := envelope.Inbound(name, sender, text, "Webhook")
	env.Account = account
	env.ThreadID = p.ThreadID
	env.ReplyTo = p.ReplyTo
	if p.ID != "" {
		env.ID = p.ID
	}
	for _, a := range p.Attachments {
		env.Attachments = append(env.Attachments, envelope.Attachment{
			Type: a.Type,
			Name: a.Name,
			MIME: a.MIME,
			URL:  a.URL,
			Size: a.Size,
		})
	}
	return env
}
