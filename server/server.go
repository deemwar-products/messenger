// Package server is messenger's small HTTP surface for `serve`: it mounts the runtime's
// channel webhooks (telegram/webhook inbound arrive on the same port) and exposes the
// consumer API — POST /send, GET /inbox?since=N, GET /health. POST /send is bearer-auth'd
// when a token is configured; the inbound webhooks carry their own per-channel HMAC/secret.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/deemwar-products/messenger/channel"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/inbox"
)

// Server wires the consumer API + the channel webhooks into one mux.
type Server struct {
	rt    *channel.Runtime
	box   *inbox.Inbox
	token string // bearer token value (resolved once at construction; "" = no auth)
}

// New builds the server over an Up'd runtime. token is the resolved bearer value for
// the consumer API ("" disables auth for loopback dev).
func New(rt *channel.Runtime, box *inbox.Inbox, token string) *Server {
	return &Server{rt: rt, box: box, token: token}
}

// Handler returns the composed mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/send", s.requireAuth(s.send))
	mux.HandleFunc("/inbox", s.requireAuth(s.inbox))
	// Mount the channel webhooks (telegram/webhook) under the same server.
	mux.Handle("/", s.rt.HTTPHandler())
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channels": s.rt.Channels()})
}

// sendReq is the POST /send body: channel + text, with optional to (thread/chat/group)
// and reply_to (message id to thread the reply onto).
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
	id, err := s.rt.Send(r.Context(), env)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// id is the provider-assigned message id — the caller's key to thread onto its own send.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
