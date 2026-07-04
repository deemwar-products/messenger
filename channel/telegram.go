package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
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
		env := envelope.Inbound(t.name, sender, u.Message.Text, "Telegram")
		env.Account = t.cfg.Account
		// ID = telegram message_id so a reply can thread to this exact message.
		if u.Message.MessageID != 0 {
			env.ID = strconv.FormatInt(u.Message.MessageID, 10)
		}
		// ThreadID = chat id so a reply lands in the same chat.
		env.ThreadID = strconv.FormatInt(u.Message.Chat.ID, 10)
		pub(env)
		w.WriteHeader(http.StatusOK)
	})
}

// Send delivers via sendMessage, threading with reply_to_message_id when ReplyTo is
// set, and returns the message_id Telegram assigned so the caller can thread onto its
// own outbound message.
func (t *telegramChannel) Send(ctx context.Context, env envelope.Envelope) (string, error) {
	token, err := t.res.Token(t.cfg)
	if err != nil {
		return "", err
	}
	base := t.cfg.Options["baseURL"]
	if base == "" {
		base = "https://api.telegram.org"
	}
	to := env.ThreadID
	if to == "" {
		to = t.cfg.Options["chatId"]
	}
	if to == "" {
		return "", fmt.Errorf("channel: telegram %q: no target (pass --to or configure --chat-id)", t.name)
	}
	form := url.Values{"chat_id": {to}, "text": {env.Text}}
	if env.ReplyTo != "" {
		form.Set("reply_to_message_id", env.ReplyTo)
	}
	// The token is a path segment, consumed only here — never logged.
	endpoint := base + "/bot" + token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("channel: telegram deliver: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("channel: telegram deliver: status %d", resp.StatusCode)
	}
	var out struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &out) == nil && out.Result.MessageID != 0 {
		return strconv.FormatInt(out.Result.MessageID, 10), nil
	}
	return "", nil
}
