// Package transport is messenger's channel I/O layer, lifted from the-factory's proven
// AI-free ingress/egress and made broker-free.
//
// Ingress: a registry of connection KINDS; every enabled one runs concurrently, each
// normalizing its source to an envelope.Envelope and handing it to a Publisher (the
// caller injects one that appends to the inbox and/or POSTs a subscriber webhook).
// Two shapes sit behind one seam:
//   - Pushed (they call us): telegram, and a generic HMAC-signed hook. Pushed kinds
//     implement Webhooker and share one HTTP server via per-path handlers.
//   - Pulled/streamed (we run them): whatsapp shells `wacli` as a long-lived
//     subprocess and reads its NDJSON stream. Such kinds implement Runner.
//
// Egress: a SenderRegistry maps a kind to its delivery adapter; `messenger send` and
// POST /send pick the adapter by channel kind and deliver, threading via ReplyTo.
//
// Secrets are use-only: a connection resolves a secret by NAME (env var or age vault
// entry) host-only at connect/send time. A value never enters config, a log, or the
// envelope.
package transport

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// Publisher is the single seam every connection uses to emit a normalized Envelope.
// The listener injects one that fans out to the inbox + webhook; tests inject a
// capturing publisher, so a connection is exercised without any sink.
type Publisher func(envelope.Envelope)

// Connection is the base every ingress kind satisfies. A concrete connection ALSO
// implements Runner (long-running) and/or Webhooker (pushed). Check reports liveness;
// Ensure brings it up idempotently.
type Connection interface {
	Kind() string
	Check() error
	Ensure() error
}

// Runner owns a long-running loop (reading a subprocess's output). Run blocks until ctx
// is cancelled or a fatal error occurs; the listener supervises it (crash = re-Ensure +
// restart).
type Runner interface {
	Connection
	Run(ctx context.Context, pub Publisher) error
}

// Webhooker is a pushed connection that answers on the shared HTTP server. Path is its
// mount path; Handler binds the publisher into an http.Handler.
type Webhooker interface {
	Connection
	Path() string
	Handler(pub Publisher) http.Handler
}

// Factory builds a connection of one kind from its config. channel is the
// [transports.<channel>] table name (also the Envelope Channel); cfg is that table.
type Factory func(channel string, cfg config.Transport) (Connection, error)

// Registry maps a connection KIND to its Factory.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{factories: map[string]Factory{}} }

// Register binds kind -> factory. A duplicate kind panics (init-order is undefined).
func (r *Registry) Register(kind string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[kind]; dup {
		panic(fmt.Sprintf("transport: kind %q registered twice", kind))
	}
	r.factories[kind] = f
}

// Kinds returns the registered kinds, sorted.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for k := range r.factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Build constructs the connection for a channel. The kind is cfg.Kind, defaulting to
// the channel name.
func (r *Registry) Build(channel string, cfg config.Transport) (Connection, error) {
	kind := cfg.Kind
	if kind == "" {
		kind = channel
	}
	r.mu.RLock()
	f := r.factories[kind]
	r.mu.RUnlock()
	if f == nil {
		return nil, fmt.Errorf("transport: no factory for kind %q (channel %q)", kind, channel)
	}
	return f(channel, cfg)
}

// DefaultRegistry is the ingress registry with the built-in kinds registered.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("telegram", newTelegramConn)
	r.Register("whatsapp", newWhatsappConn)
	r.Register("hook", newHookConn)
	return r
}
