package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// Sender delivers ONE outbound Envelope on its channel. The caller picks the adapter by
// the connection kind, targets by the self-describing envelope (ThreadID = where,
// ReplyTo = which message to thread), and resolves the one needed secret by NAME via
// resolver at the point of the API call. A secret value never enters a log or the message.
type Sender interface {
	Kind() string
	Send(ctx context.Context, env envelope.Envelope, cfg config.Transport, resolver *SecretResolver) error
}

// SenderRegistry maps a kind to its delivery adapter.
type SenderRegistry struct {
	mu      sync.RWMutex
	senders map[string]Sender
}

// NewSenderRegistry returns an empty registry.
func NewSenderRegistry() *SenderRegistry { return &SenderRegistry{senders: map[string]Sender{}} }

// DefaultSenderRegistry has the built-in delivery adapters registered.
func DefaultSenderRegistry() *SenderRegistry {
	s := NewSenderRegistry()
	s.Register(telegramSender{})
	s.Register(whatsappSender{})
	s.Register(hookSender{})
	return s
}

// Register binds a sender to its Kind(). A duplicate panics.
func (s *SenderRegistry) Register(sender Sender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := sender.Kind()
	if _, dup := s.senders[k]; dup {
		panic(fmt.Sprintf("transport: sender kind %q registered twice", k))
	}
	s.senders[k] = sender
}

// Get returns the sender for kind (nil, false if none).
func (s *SenderRegistry) Get(kind string) (Sender, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snd, ok := s.senders[kind]
	return snd, ok
}

// Kinds lists registered delivery kinds, sorted.
func (s *SenderRegistry) Kinds() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.senders))
	for k := range s.senders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var senderHTTP = &http.Client{Timeout: 15 * time.Second}

// --- telegram -------------------------------------------------------------------------

type telegramSender struct{}

func (telegramSender) Kind() string { return "telegram" }

func (telegramSender) Send(ctx context.Context, env envelope.Envelope, cfg config.Transport, resolver *SecretResolver) error {
	token, err := resolver.Token(cfg)
	if err != nil {
		return err
	}
	base := cfg.Options["baseURL"]
	if base == "" {
		base = "https://api.telegram.org"
	}
	form := url.Values{"chat_id": {env.ThreadID}, "text": {env.Text}}
	// Thread the reply to a specific message when ReplyTo is set.
	if env.ReplyTo != "" {
		form.Set("reply_to_message_id", env.ReplyTo)
	}
	// The token is a path segment, consumed only here — never logged.
	endpoint := base + "/bot" + token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doDeliver(req, "telegram")
}

// --- hook (callback) ------------------------------------------------------------------

type hookSender struct{}

func (hookSender) Kind() string { return "hook" }

func (hookSender) Send(ctx context.Context, env envelope.Envelope, cfg config.Transport, resolver *SecretResolver) error {
	callback := cfg.Options["callbackURL"]
	if callback == "" {
		return fmt.Errorf("transport: hook: no callbackURL")
	}
	body, _ := json.Marshal(env)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callback, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.TokenEnv != "" || cfg.TokenVault != "" {
		secret, serr := resolver.Token(cfg)
		if serr != nil {
			return serr
		}
		req.Header.Set("X-Hub-Signature-256", signHMAC([]byte(secret), body))
	}
	return doDeliver(req, "hook")
}

// --- whatsapp (wacli send) ------------------------------------------------------------

type whatsappSender struct{}

func (whatsappSender) Kind() string { return "whatsapp" }

func (whatsappSender) Send(ctx context.Context, env envelope.Envelope, cfg config.Transport, _ *SecretResolver) error {
	bin := cfg.Options["bin"]
	if bin == "" {
		bin = "wacli"
	}
	to := env.ThreadID
	if to == "" {
		return fmt.Errorf("transport: whatsapp: no recipient (thread id)")
	}
	args := []string{"send", "text", "--to", to, "--message", env.Text}
	// Quote the message being replied to when ReplyTo is set (wacli --quote <id>).
	if env.ReplyTo != "" {
		args = append(args, "--quote", env.ReplyTo)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("transport: whatsapp: wacli send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// doDeliver performs the request and maps a non-2xx to an error WITHOUT echoing the
// request (which may carry a secret in a header or URL).
func doDeliver(req *http.Request, kind string) error {
	resp, err := senderHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("transport: %s deliver: %w", kind, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("transport: %s deliver: status %d", kind, resp.StatusCode)
	}
	return nil
}
