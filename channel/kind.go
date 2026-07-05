package channel

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/deemwar-products/messenger/config"
)

// Kind is the polymorphic face of one channel kind (whatsapp / telegram / webhook /
// tomorrow: teams, slack, …). A kind owns BOTH its wire behavior (Open, and Streaming
// when it has a shared inbound stream) and its CLI behavior (validate/hints/connect/
// test/detail/lane/status), so the whole kind — personality included — lives in ONE
// file, and the CLI dispatches with zero kind conditionals.
//
// Embed Base to inherit neutral defaults for everything but Name, Traits, and Open.
type Kind interface {
	Name() string
	Traits() Traits
	// Open builds the per-channel value (Send + optionally Pushed).
	Open(name string, cfg config.Transport, res *SecretResolver) (Channel, error)

	// Validate vets a new channel's config against the existing ones at `channel add`
	// time (whatsapp: one group JID = one channel).
	Validate(name string, cfg config.Transport, existing map[string]config.Transport) error
	// AddHints returns the next-step guidance printed after `channel add`.
	AddHints(name string, cfg config.Transport) []string
	// Connect performs the kind's connect/pair step for `channel connect` (may be
	// interactive). It never handles a secret VALUE — names only.
	Connect(name string, cfg config.Transport, p ConnectParams) error
	// Test probes connectivity WITHOUT sending a message; returns human-readable
	// lines. A secret value never appears in them.
	Test(ctx context.Context, name string, cfg config.Transport, res *SecretResolver) ([]string, error)
	// Detail is the one-line target shown in `channel list` (group=… / chat=… / path=…).
	Detail(name string, cfg config.Transport) string
	// Lane builds an agent's OWN channel of this kind for `messenger register`,
	// returning the channel config plus onboarding hints.
	Lane(name string, p LaneParams, existing map[string]config.Transport) (config.Transport, []string, error)
	// Status reports host-level kind state for `setup`/`status` (whatsapp: the global
	// device's pair state). Nil = nothing host-level to report.
	Status() []string
}

// Streaming is the capability of kinds whose inbound is ONE shared long-lived stream
// serving every channel of the kind (whatsapp: the single wacli subprocess). The
// runtime asserts it — mirroring how Pushed is asserted on channels.
type Streaming interface {
	OpenStream(chans map[string]config.Transport, res *SecretResolver) (Streamer, error)
}

// WebhookInbound is a Streamer whose inbound ALSO arrives over an HTTP webhook the
// runtime mounts on the shared server (whatsapp: `wacli sync --webhook` POSTs live
// messages to us — sync's stdout carries none). The runtime mounts Handler at Path and
// injects the loopback URL the subprocess must POST to via UseCallback.
type WebhookInbound interface {
	Streamer
	Path() string
	Handler(pub Publisher) http.Handler
	UseCallback(url string)
}

// Traits are the kind's declarative facts the CLI reads (not behavior).
type Traits struct {
	RequiresToken bool   // channel add must name a token (env or vault)
	TargetFlag    string // the add-flag naming the default target ("chat-id", "group", "")
}

// ConnectParams carry `channel connect` flags + context a kind may need.
type ConnectParams struct {
	PublicURL string                      // telegram: public base URL for the setWebhook print
	Existing  map[string]config.Transport // all configured channels (whatsapp: hide already-bound groups)
}

// LaneParams are the register-flag inputs a kind may use to build an agent lane.
// Secrets are NAMES only, as everywhere.
type LaneParams struct {
	Group    string // whatsapp: the group JID the lane is bound to
	TokenEnv string // telegram bot token / webhook HMAC secret, by NAME
	ChatID   string // telegram: default target chat
}

// Base provides neutral defaults so a kind implements only what it has. Embed it.
type Base struct{}

func (Base) Traits() Traits                                                       { return Traits{} }
func (Base) Validate(string, config.Transport, map[string]config.Transport) error { return nil }
func (Base) AddHints(string, config.Transport) []string                           { return nil }
func (Base) Connect(name string, _ config.Transport, _ ConnectParams) error {
	fmt.Printf("channel %q needs no connect step\n", name)
	return nil
}
func (Base) Test(context.Context, string, config.Transport, *SecretResolver) ([]string, error) {
	return []string{"nothing to test"}, nil
}
func (Base) Detail(string, config.Transport) string { return "" }
func (Base) Lane(name string, _ LaneParams, _ map[string]config.Transport) (config.Transport, []string, error) {
	return config.Transport{}, nil, fmt.Errorf("this kind cannot host agent lanes (use --channels to attach to existing ones)")
}
func (Base) Status() []string { return nil }

// LaneMatches reports whether an existing channel already IS the wanted lane —
// register's idempotency check: same kind and same targets (where the want sets one).
func LaneMatches(name string, have, want config.Transport) bool {
	if NormalizeKind(have.Kind, name) != NormalizeKind(want.Kind, name) {
		return false
	}
	for k, v := range want.Options {
		if have.Options[k] != v {
			return false
		}
	}
	if want.TokenEnv != "" && have.TokenEnv != want.TokenEnv {
		return false
	}
	return true
}

// kinds is the mutable kind registry. Built-ins register from their own file's init();
// a new kind (teams, slack, …) is ONE file with a type embedding Base plus a Register
// call — never a parallel registry, never a CLI edit.
var (
	kindsMu sync.RWMutex
	kinds   = map[string]Kind{}
)

// Register adds a kind to the registry. A duplicate name panics (init-order bug).
func Register(k Kind) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if k == nil || k.Name() == "" {
		panic("channel: Register needs a named Kind")
	}
	if _, dup := kinds[k.Name()]; dup {
		panic(fmt.Sprintf("channel: kind %q registered twice", k.Name()))
	}
	kinds[k.Name()] = k
}

// Kinds returns a snapshot of the registry, keyed by canonical name.
func Kinds() map[string]Kind {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	out := make(map[string]Kind, len(kinds))
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

// NormalizeKind maps a configured kind to its canonical name. "hook" is the legacy
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

// KindFor resolves the Kind for a channel's config.
func KindFor(name string, cfg config.Transport) (Kind, error) {
	kind := NormalizeKind(cfg.Kind, name)
	k, ok := Kinds()[kind]
	if !ok {
		return nil, fmt.Errorf("channel: unknown kind %q (channel %q)", kind, name)
	}
	return k, nil
}
