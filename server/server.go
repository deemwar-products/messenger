// Package server is messenger's small HTTP surface for `serve`: it mounts the runtime's
// channel webhooks (telegram/webhook inbound arrive on the same port) and exposes the
// consumer API — POST /send, GET /inbox?since=N, GET /media/<basename>, GET /health.
// POST /send is bearer-auth'd when a token is configured; the inbound webhooks carry
// their own per-channel HMAC/secret.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/deemwar-products/messenger/channel"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/home"
	"github.com/deemwar-products/messenger/inbox"
)

// defaultHookSecretEnv is the env NAME the universal hook resolves its shared secret from
// when config leaves HookSecretEnv empty. The VALUE lives in the process env only.
const defaultHookSecretEnv = "MESSENGER_HOOK_SECRET"

// Server wires the consumer API + the channel webhooks into one mux.
type Server struct {
	rt            *channel.Runtime
	box           *inbox.Inbox
	token         string // bearer token value (resolved once at construction; "" = no auth)
	hookSecretEnv string // NAME of the env var holding the universal-hook shared secret
}

// New builds the server over an Up'd runtime. token is the resolved bearer value for
// the consumer API ("" disables auth for loopback dev).
func New(rt *channel.Runtime, box *inbox.Inbox, token string) *Server {
	return &Server{rt: rt, box: box, token: token, hookSecretEnv: defaultHookSecretEnv}
}

// UseHookSecret overrides the env NAME the universal hook reads its shared secret from
// (config's HookSecretEnv). Empty keeps the default MESSENGER_HOOK_SECRET. The value is
// never held — it is resolved from the env per request.
func (s *Server) UseHookSecret(envName string) {
	if envName != "" {
		s.hookSecretEnv = envName
	}
}

// Handler returns the composed mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/send", s.requireAuth(s.send))
	mux.HandleFunc("/inbox", s.requireAuth(s.inbox))
	mux.HandleFunc("/media/", s.requireAuth(s.media))
	// The universal hook: one HMAC-signed pair any peer messenger uses symmetrically —
	// /hook/send pushes a message IN, /hook/recv polls messages OUT. Exact patterns win
	// over the "/" catch-all, so a peer needs no per-lane config to reach the hub.
	mux.HandleFunc("/hook/send", s.hookSend)
	mux.HandleFunc("/hook/recv", s.hookRecv)
	// Mount the channel webhooks (telegram/webhook) under the same server.
	mux.Handle("/", s.rt.HTTPHandler())
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	// "service" self-identifies the hub so the single-instance probe (and any client)
	// can tell a running messenger from an unrelated server on the same port.
	// "streams" reports per-kind liveness of supervised streaming channels (whatsapp's one
	// wacli subprocess) so a monitor can see a listener that died or is looping in backoff
	// without host-process forensics. Empty when no streaming channels are configured.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "service": "messenger",
		"channels": s.rt.Channels(),
		"streams":  s.rt.StreamHealth(),
	})
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

// requireUnderMediaDir rejects any local attachment path that does not resolve inside
// home.MediaDir() — the same directory GET /media already trusts, and exactly where
// inbound attachments are stored (so forwarding a received attachment still works).
// Without this, POST /send would let any caller who can reach the HTTP API read and
// exfiltrate an arbitrary file on the host (e.g. {"file":"/etc/passwd"}).
func requireUnderMediaDir(path string) error {
	mediaDir, err := filepath.Abs(home.MediaDir())
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(mediaDir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errAttachmentOutsideMediaDir
	}
	return nil
}

var errAttachmentOutsideMediaDir = errors.New("attachment path must be under the media directory — use an http(s) url for external files")

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
	for _, a := range attachments {
		if a.Path == "" || a.URL != "" {
			continue
		}
		if err := requireUnderMediaDir(a.Path); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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
		// A channel that is inbound-only (no outbound target) is a config precondition, not
		// a gateway failure: answer 422 with a structured, actionable body so a caller fixes
		// config instead of retrying a 502 forever. Genuine delivery failures stay 502.
		if errors.Is(err, channel.ErrNoOutbound) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"ok": false, "error": err.Error(), "channel": req.Channel, "reason": "inbound_only",
			})
			return
		}
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

// hookSendReq is the routing envelope of the universal hook's POST /hook/send. It rides
// alongside the standard webhook payload (text/sender/thread_id/reply_to/id/attachments):
// `channel` names the lane the message belongs to (defaults to "hook"); `deliver` also
// routes it OUT through that channel (like /send) instead of only injecting it inbound.
type hookSendReq struct {
	Channel string `json:"channel"`
	Deliver bool   `json:"deliver"`
}

// hookSend is the IN half of the symmetric hook: a signed peer POSTs a message and it is
// injected into the hub's inbound seam (inbox + subscriptions), or — with deliver:true —
// routed out through the named channel. Auth is HMAC X-Hub-Signature-256 over the raw body
// with the shared hook secret. Idempotency is the caller's `id`, deduped by consumers.
func (s *Server) hookSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, ok := s.readSignedHook(w, r)
	if !ok {
		return
	}
	var meta hookSendReq
	_ = json.Unmarshal(body, &meta)
	name := meta.Channel
	if name == "" {
		name = "hook"
	}
	// Same lenient payload shape as a per-lane /webhook/<name>; a remote `path` is never
	// trusted (attachments carry url only) — reusing NormalizeWebhook keeps one path.
	env := channel.NormalizeWebhook(name, "", body)
	if meta.Deliver {
		id, err := s.rt.Send(r.Context(), env)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "delivered": true})
		return
	}
	s.rt.Publish(env)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "id": env.ID})
}

// hookRecvReq is the POST /hook/recv body: read every inbound message after `since`
// (a 1-based cursor the peer persists), optionally filtered to `channels`.
type hookRecvReq struct {
	Since    int      `json:"since"`
	Channels []string `json:"channels"`
}

// hookRecv is the OUT half of the symmetric hook: a signed peer polls inbound messages
// since its own cursor (channel-filterable), the same store /inbox reads but reachable
// with the shared hook secret instead of the serve bearer. Simpler than push-callback and
// stateless on the hub; peers wanting push register a subscription callbackURL instead.
func (s *Server) hookRecv(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, ok := s.readSignedHook(w, r)
	if !ok {
		return
	}
	var req hookRecvReq
	_ = json.Unmarshal(body, &req)
	msgs, next, err := s.box.Since(req.Since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(req.Channels) > 0 {
		filtered := msgs[:0]
		for _, m := range msgs {
			for _, c := range req.Channels {
				if m.Channel == c {
					filtered = append(filtered, m)
					break
				}
			}
		}
		msgs = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "messages": msgs, "next": next})
}

// readSignedHook reads the (capped) body and verifies the shared-secret HMAC. It resolves
// the secret by NAME from the env per request — the value never enters the Server, a log,
// or the response. Unset secret → 503 (hook disabled), bad signature → 401.
func (s *Server) readSignedHook(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	secret := os.Getenv(s.hookSecretEnv)
	if secret == "" {
		http.Error(w, "hook disabled: no shared secret configured", http.StatusServiceUnavailable)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return nil, false
	}
	if !channel.VerifyHMAC([]byte(secret), body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return nil, false
	}
	return body, true
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
