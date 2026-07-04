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
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

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

// KindSpec captures one kind's rules: how many channels it supports, what config it
// requires, how to open a channel, and (for Shared kinds) how to open the ONE stream
// that serves all of them.
type KindSpec struct {
	Name          string
	Shared        bool   // one runtime stream serves ALL channels of this kind
	RequiresToken bool   // channel add must name a token (env or vault)
	TargetFlag    string // the add-flag naming the default target ("chat-id", "group", "")

	// Open builds the per-channel value (Send + optionally Pushed).
	Open func(name string, cfg config.Transport, res *SecretResolver) (Channel, error)
	// OpenStream builds the kind's single shared inbound stream over every enabled
	// channel of this kind. Nil for pushed-only kinds.
	OpenStream func(chans map[string]config.Transport, res *SecretResolver) (Streamer, error)
	// Test probes connectivity for one channel WITHOUT sending a message: whatsapp
	// checks the global device, telegram calls getMe with the token (by NAME),
	// webhook verifies the secret is resolvable. It returns human-readable lines;
	// a secret value never appears in them. Nil = nothing to test.
	Test func(ctx context.Context, name string, cfg config.Transport, res *SecretResolver) ([]string, error)
}

// kinds is the mutable kind registry. Built-ins register in init(); a new kind
// (teams, slack, …) is ONE file implementing Channel (+ Pushed or a Streamer) plus a
// Register call — never a parallel registry.
var (
	kindsMu sync.RWMutex
	kinds   = map[string]KindSpec{}
)

// Register adds a kind to the registry. A duplicate name panics (init-order bug).
func Register(spec KindSpec) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if spec.Name == "" || spec.Open == nil {
		panic("channel: Register needs Name and Open")
	}
	if _, dup := kinds[spec.Name]; dup {
		panic(fmt.Sprintf("channel: kind %q registered twice", spec.Name))
	}
	kinds[spec.Name] = spec
}

func init() {
	Register(KindSpec{
		Name: "telegram", RequiresToken: true, TargetFlag: "chat-id",
		Open: openTelegram,
		Test: testTelegram,
	})
	Register(KindSpec{
		Name: "whatsapp", Shared: true, TargetFlag: "group",
		Open:       openWhatsapp,
		OpenStream: openWhatsappStream,
		Test:       testWhatsapp,
	})
	Register(KindSpec{
		Name: "webhook", RequiresToken: true,
		Open: openWebhook,
		Test: testWebhook,
	})
}

// Kinds returns a snapshot of the registry, keyed by canonical name.
func Kinds() map[string]KindSpec {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	out := make(map[string]KindSpec, len(kinds))
	for k, v := range kinds {
		out[k] = v
	}
	return out
}

// KindNames returns the canonical kind names, sorted.
func KindNames() []string {
	ks := Kinds()
	out := make([]string, 0, len(ks))
	for k := range ks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NormalizeKind maps a configured kind to its canonical spec name. "hook" is the legacy
// alias for "webhook"; an empty kind defaults to the channel name.
func NormalizeKind(kind, channelName string) string {
	if kind == "" {
		kind = channelName
	}
	if kind == "hook" {
		return "webhook"
	}
	return kind
}

// SpecFor resolves the KindSpec for a channel's config.
func SpecFor(name string, cfg config.Transport) (KindSpec, error) {
	kind := NormalizeKind(cfg.Kind, name)
	spec, ok := Kinds()[kind]
	if !ok {
		return KindSpec{}, fmt.Errorf("channel: unknown kind %q (channel %q)", kind, name)
	}
	return spec, nil
}
