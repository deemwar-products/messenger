package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// telegramConn is the Telegram Bot API webhook. Telegram POSTs an Update to the
// registered webhook URL; this handler normalizes the message to an inbound Envelope.
// The bot token is referenced by NAME and only the sender adapter needs its value.
// Telegram optionally signs the webhook with a secret_token header; when
// options["secretHeader"] and a token secret are configured, it is verified.
type telegramConn struct {
	channel  string
	cfg      config.Transport
	resolver *SecretResolver
}

func newTelegramConn(channel string, cfg config.Transport) (Connection, error) {
	return &telegramConn{channel: channel, cfg: cfg, resolver: NewSecretResolver(nil)}, nil
}

func (t *telegramConn) Kind() string  { return "telegram" }
func (t *telegramConn) Check() error  { return nil }
func (t *telegramConn) Ensure() error { return nil }

func (t *telegramConn) Path() string {
	if p := t.cfg.Options["path"]; p != "" {
		return p
	}
	return "/telegram/" + t.channel
}

// tgUpdate is the slice of the Telegram Update we normalize. MessageID is captured so a
// reply can thread via reply_to_message_id.
type tgUpdate struct {
	Message struct {
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		From      struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

func (t *telegramConn) Handler(pub Publisher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if hd := t.cfg.Options["secretHeader"]; hd != "" {
			want, err := t.resolver.Token(t.cfg)
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
		env := envelope.Inbound(t.channel, sender, u.Message.Text, "Telegram")
		env.Account = t.cfg.Account
		// ID = telegram message_id so a reply can thread to this exact message.
		if u.Message.MessageID != 0 {
			env.ID = strconv.FormatInt(u.Message.MessageID, 10)
		}
		// ThreadID = chat id so the sender delivers the reply to the same chat.
		env.ThreadID = strconv.FormatInt(u.Message.Chat.ID, 10)
		pub(env)
		w.WriteHeader(http.StatusOK)
	})
}
