package transport

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// hookConn is the generic HMAC-signed inbound webhook: "so anything can call in". A
// caller POSTs a body plus an X-Hub-Signature-256 header (sha256=<hex>) computed over
// the body with the shared secret. A verified payload becomes an inbound Envelope; an
// unverified one is rejected 401. The secret is resolved by NAME — its value never
// enters config, a log, or the envelope.
type hookConn struct {
	channel  string
	cfg      config.Transport
	resolver *SecretResolver
}

func newHookConn(channel string, cfg config.Transport) (Connection, error) {
	return &hookConn{channel: channel, cfg: cfg, resolver: NewSecretResolver(nil)}, nil
}

func (h *hookConn) Kind() string { return "hook" }

func (h *hookConn) Check() error  { return nil }
func (h *hookConn) Ensure() error { return nil }

// Path defaults to /hook/<channel>; overridable via options["path"].
func (h *hookConn) Path() string {
	if p := h.cfg.Options["path"]; p != "" {
		return p
	}
	return "/hook/" + h.channel
}

func (h *hookConn) signatureHeader() string {
	if hd := h.cfg.Options["signatureHeader"]; hd != "" {
		return hd
	}
	return "X-Hub-Signature-256"
}

func (h *hookConn) Handler(pub Publisher) http.Handler {
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
		secret, err := h.resolver.Token(h.cfg)
		if err != nil {
			http.Error(w, "unconfigured", http.StatusInternalServerError)
			return
		}
		if !verifyHMAC([]byte(secret), body, r.Header.Get(h.signatureHeader())) {
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
			return
		}
		pub(normalizeHook(h.channel, h.cfg.Account, body))
		w.WriteHeader(http.StatusAccepted)
	})
}

// signHMAC returns the sha256=<hex> signature of body under secret.
func signHMAC(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// verifyHMAC is the constant-time check.
func verifyHMAC(secret, body []byte, sig string) bool {
	if sig == "" {
		return false
	}
	return hmac.Equal([]byte(signHMAC(secret, body)), []byte(sig))
}

// normalizeHook turns a verified payload into the canonical inbound Envelope. It reads a
// best-effort {text|message, sender|login, thread_id, reply_to, id} shape and falls back
// to the raw body as text, so any signed caller can inject a message (and thread it).
func normalizeHook(channel, account string, body []byte) envelope.Envelope {
	var p struct {
		Text     string `json:"text"`
		Message  string `json:"message"`
		Sender   string `json:"sender"`
		Login    string `json:"login"`
		ThreadID string `json:"thread_id"`
		ReplyTo  string `json:"reply_to"`
		ID       string `json:"id"`
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
	env := envelope.Inbound(channel, sender, text, "Hook")
	env.Account = account
	env.ThreadID = p.ThreadID
	env.ReplyTo = p.ReplyTo
	if p.ID != "" {
		env.ID = p.ID
	}
	return env
}
