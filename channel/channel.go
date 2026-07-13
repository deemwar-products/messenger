// Package channel is messenger's channel layer: ONE common interface per channel kind
// (telegram / whatsapp / webhook), covering both ingress and egress, with per-kind
// multiplicity rules captured in a KindSpec:
//
//   - telegram: many channels, each its OWN bot (own token by NAME) + default chat id.
//   - whatsapp: many named GROUP channels sharing ONE global paired device — exactly one
//     `wacli sync --follow` stream runs regardless of channel count; inbound is routed
//     to the channel whose group JID matches the chat; sends target the channel's group.
//   - webhook: many channels, each its own HMAC-signed inbound path + secret.
//
// Ingress shapes: a Pushed channel mounts an http.Handler (they call us); a Shared kind
// exposes one Streamer for all its channels (we run it, supervised). Egress: every
// channel Sends and returns the provider-assigned message id so consumers can thread
// onto their own outbound messages.
//
// Secrets are use-only: resolved by NAME (env var or age vault entry) host-only at the
// point of the API call. A value never enters config, a log, or the envelope.
package channel

import (
	"context"
	"errors"
	"net/http"

	"github.com/deemwar-products/messenger/envelope"
)

// ErrNoOutbound marks a Send against a channel that is configured inbound-only and so can
// never deliver outbound (a webhook with no callbackURL). It is a configuration
// precondition — retrying the same request cannot succeed — so the HTTP surface maps it to
// a 4xx (422) rather than a 502 gateway failure, which would read as a transient upstream
// fault and invite pointless retries. Distinguish it with errors.Is(err, ErrNoOutbound).
var ErrNoOutbound = errors.New("channel: inbound-only, no outbound target configured")

// Publisher is the single seam every ingress path uses to emit a normalized Envelope.
// The runtime injects one that fans out to the inbox + subscriptions; tests inject a
// capturing publisher.
type Publisher func(envelope.Envelope)

// Channel is the ONE interface every configured channel satisfies — ingress and egress
// live on the same type. Send delivers env (ThreadID = where, ReplyTo = which message to
// thread onto) and returns the provider-assigned message id ("" falls back to env.ID).
type Channel interface {
	Name() string
	Kind() string
	Send(ctx context.Context, env envelope.Envelope) (providerID string, err error)
}

// Pushed is a channel whose inbound is HTTP-pushed to us (telegram, webhook). Path is
// its mount path on the shared server; Handler binds the publisher.
type Pushed interface {
	Channel
	Path() string
	Handler(pub Publisher) http.Handler
}

// Streamer is a kind-level shared inbound stream (whatsapp: the one wacli subprocess
// serving every configured group channel). Run blocks until ctx is cancelled or a fatal
// error occurs; the runtime supervises it with backoff.
type Streamer interface {
	Run(ctx context.Context, pub Publisher) error
}

// The kind registry, the Kind interface (one polymorphic type per kind, CLI behavior
// included), and its Base defaults live in kind.go.
