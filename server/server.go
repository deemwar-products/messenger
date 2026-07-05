// Package server is messenger's small HTTP surface for `serve`: it mounts the runtime's
// channel webhooks (telegram/webhook inbound arrive on the same port) and exposes the
// consumer API — POST /send, GET /inbox?since=N, GET /media/<basename>, GET /health.
// POST /send is bearer-auth'd when a token is configured; the inbound webhooks carry
// their own per-channel HMAC/secret.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/deemwar-products/messenger/channel"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/home"
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
	mux.HandleFunc("/media/", s.requireAuth(s.media))
	// Mount the channel webhooks (telegram/webhook) under the same server.
	mux.Handle("/", s.rt.HTTPHandler())
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	// "service" self-identifies the hub so the single-instance probe (and any client)
	// can tell a running messenger from an unrelated server on the same port.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "messenger", "channels": s.rt.Channels()})
}

// sendReq is the POST /send body: channel plus text and/or attachments, with optional
// to (thread/chat/group) and reply_to (message id to thread the reply onto). `file` is
// the one-attachment shorthand: a local path or http(s) URL that becomes attachments[0].
type sendReq struct {
	Channel     string                `json:"channel"`
	Text        string                `json:"text"`
	To          string                `json:"to"`
	ReplyTo     string                `json:"reply_to"`
	File        string                `json:"file"`
	Attachments []envelope.Attachment `json:"attachments"`
}

// fileAttachment builds the shorthand attachment: a remote http(s) reference rides as
// URL, anything else is a local Path. Name is the base name, Type the generic "file".
func fileAttachment(file string) envelope.Attachment {
	a := envelope.Attachment{Type: "file", Name: filepath.Base(file)}
	if strings.HasPrefix(file, "http://") || strings.HasPrefix(file, "https://") {
		a.URL = file
	} else {
		a.Path = file
	}
	return a
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
	attachments := req.Attachments
	if req.File != "" {
		attachments = append(attachments, fileAttachment(req.File))
	}
	if req.Channel == "" || (req.Text == "" && len(attachments) == 0) {
		http.Error(w, "channel and text (or an attachment) are required", http.StatusBadRequest)
		return
	}
	// Conversation-first: reply_to "last" resolves to the newest inbound message on
	// this channel (and thread, when given) and inherits its thread — "answer the
	// obvious previous message" without the caller bookkeeping ids.
	replyTo, to := req.ReplyTo, req.To
	if replyTo == "last" {
		last, ok, lerr := s.box.Last(req.Channel, req.To)
		if lerr != nil {
			http.Error(w, lerr.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no previous message on channel "+req.Channel+" to reply to", http.StatusConflict)
			return
		}
		replyTo = last.ID
		if to == "" {
			to = last.ThreadID
		}
	}
	env := envelope.Normalize(envelope.Envelope{
		Channel:     req.Channel,
		Text:        req.Text,
		ThreadID:    to,
		ReplyTo:     replyTo,
		Origin:      "messenger",
		Attachments: attachments,
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

// media serves one stored attachment: GET /media/<basename> streams the file from the
// home media dir. SECURITY: only the final path element is honored — anything empty,
// dot-only, or still containing a separator after trimming the prefix is rejected, so a
// traversal outside the media dir is impossible. Absent files are 404.
func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/media/")
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\") || name != filepath.Base(name) {
		http.Error(w, "bad media name", http.StatusBadRequest)
		return
	}
	path := filepath.Join(home.MediaDir(), name)
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	f, err := http.Dir(home.MediaDir()).Open(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", ct)
	http.ServeContent(w, r, path, st.ModTime(), f)
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
