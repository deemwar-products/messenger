// Package server is messenger's small HTTP surface for `serve`: it mounts the listener's
// channel webhooks (so telegram/hook inbound arrive on the same port) and exposes the
// consumer API — POST /send, GET /inbox?since=N, GET /health. POST /send is bearer-auth'd
// when a token is configured; the inbound webhooks carry their own per-channel HMAC/secret.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/inbox"
	"github.com/deemwar-products/messenger/transport"
)

// Server wires the consumer API + the channel webhooks into one mux.
type Server struct {
	cfg      *config.Config
	box      *inbox.Inbox
	senders  *transport.SenderRegistry
	resolver *transport.SecretResolver
	token    string // bearer token value (resolved once at construction; "" = no auth)
	webhooks http.Handler
}

// New builds the server. webhooks is the listener's HTTPHandler (channel inbound); token
// is the resolved bearer value for POST /send ("" disables auth for loopback dev).
func New(cfg *config.Config, box *inbox.Inbox, senders *transport.SenderRegistry, resolver *transport.SecretResolver, token string, webhooks http.Handler) *Server {
	return &Server{cfg: cfg, box: box, senders: senders, resolver: resolver, token: token, webhooks: webhooks}
}

// Handler returns the composed mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/send", s.requireAuth(s.send))
	mux.HandleFunc("/inbox", s.requireAuth(s.inbox))
	// Mount the channel webhooks (telegram/hook) under the same server.
	if s.webhooks != nil {
		mux.Handle("/", s.webhooks)
	}
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channels": s.senders.Kinds()})
}

// sendReq is the POST /send body: channel + text, with optional to (thread/chat) and
// replyTo (message id to thread the reply onto).
type sendReq struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
	To      string `json:"to"`
	ReplyTo string `json:"reply_to"`
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req sendReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if req.Channel == "" || req.Text == "" {
		http.Error(w, "channel and text are required", http.StatusBadRequest)
		return
	}
	env := envelope.Normalize(envelope.Envelope{
		Channel:  req.Channel,
		Text:     req.Text,
		ThreadID: req.To,
		ReplyTo:  req.ReplyTo,
		Origin:   "messenger",
	})
	if err := Deliver(r.Context(), s.cfg, s.senders, s.resolver, env); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": env.ID})
}

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	since := 0
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			since = n
		}
	}
	msgs, next, err := s.box.Since(since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "next": next})
}

// requireAuth enforces the bearer token on the consumer API when one is configured.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := r.Header.Get("Authorization")
			want := "Bearer " + s.token
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// Deliver resolves the channel's config + kind and delivers env via the matching sender.
func Deliver(ctx context.Context, cfg *config.Config, senders *transport.SenderRegistry, resolver *transport.SecretResolver, env envelope.Envelope) error {
	tcfg, ok := cfg.Transports[env.Channel]
	if !ok {
		return &deliverErr{"unknown channel: " + env.Channel}
	}
	kind := tcfg.Kind
	if kind == "" {
		kind = env.Channel
	}
	snd, ok := senders.Get(kind)
	if !ok {
		return &deliverErr{"no sender for kind: " + kind}
	}
	return snd.Send(ctx, env, tcfg, resolver)
}

type deliverErr struct{ msg string }

func (e *deliverErr) Error() string { return e.msg }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
