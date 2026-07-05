package channel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// Runtime owns every enabled channel: it opens each via its KindSpec, mounts Pushed
// handlers on one shared mux, runs each Shared kind's ONE stream (whatsapp: one wacli
// subprocess for all group channels) supervised with exponential backoff, and routes
// Send by channel name. Per-channel/stream failure is isolated.
type Runtime struct {
	res     *SecretResolver
	pub     Publisher
	seed    map[string]config.Transport
	selfURL string // hub loopback base URL, injected into WebhookInbound streams (wacli)

	mu       sync.Mutex
	channels map[string]Channel
	mux      *http.ServeMux
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	restartMin time.Duration
	restartMax time.Duration
}

// NewRuntime builds a runtime over the enabled channel configs. pub is where every
// inbound Envelope lands (the caller injects the inbox+subscription fan-out).
func NewRuntime(seed map[string]config.Transport, res *SecretResolver, pub Publisher) *Runtime {
	if seed == nil {
		seed = map[string]config.Transport{}
	}
	if res == nil {
		res = NewSecretResolver(nil)
	}
	return &Runtime{
		res: res, pub: pub, seed: seed,
		channels:   map[string]Channel{},
		mux:        http.NewServeMux(),
		restartMin: 500 * time.Millisecond,
		restartMax: 30 * time.Second,
	}
}

// HTTPHandler is the shared server for Pushed channels, composable into `serve`.
func (rt *Runtime) HTTPHandler() http.Handler { return rt.mux }

// SetSelfURL tells the runtime its own loopback base URL (e.g. http://127.0.0.1:14310)
// so WebhookInbound streams (wacli) can point their subprocess back at the hub. Call
// before Up.
func (rt *Runtime) SetSelfURL(u string) { rt.selfURL = u }

// Channels returns name -> kind for everything opened (health/introspection).
func (rt *Runtime) Channels() map[string]string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := map[string]string{}
	for n, c := range rt.channels {
		out[n] = c.Kind()
	}
	return out
}

// Up opens every enabled channel, mounts the Pushed ones, and starts one supervised
// stream per Shared kind. A single channel's failure is collected, never fatal to the
// others.
func (rt *Runtime) Up(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	rt.mu.Lock()
	rt.cancel = cancel
	rt.mu.Unlock()

	var errs []error
	sharedKinds := map[string]map[string]config.Transport{}

	for name, cfg := range rt.seed {
		k, err := KindFor(name, cfg)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ch, err := k.Open(name, cfg, rt.res)
		if err != nil {
			errs = append(errs, fmt.Errorf("channel: open %q: %w", name, err))
			continue
		}
		rt.mu.Lock()
		rt.channels[name] = ch
		rt.mu.Unlock()
		if p, ok := ch.(Pushed); ok {
			rt.mux.Handle(p.Path(), p.Handler(rt.pub))
			// Legacy alias: a default /webhook/<name> also answers on /hook/<name>.
			if ch.Kind() == "webhook" && cfg.Options["path"] == "" {
				rt.mux.Handle("/hook/"+name, p.Handler(rt.pub))
			}
		}
		// Streaming kinds get ONE shared inbound stream over all their channels.
		if _, ok := k.(Streaming); ok {
			if sharedKinds[k.Name()] == nil {
				sharedKinds[k.Name()] = map[string]config.Transport{}
			}
			sharedKinds[k.Name()][name] = cfg
		}
	}

	// One stream per streaming kind, no matter how many channels of that kind exist.
	for kind, chans := range sharedKinds {
		st, err := Kinds()[kind].(Streaming).OpenStream(chans, rt.res)
		if err != nil {
			errs = append(errs, fmt.Errorf("channel: %s stream: %w", kind, err))
			continue
		}
		// A stream that receives over an HTTP webhook (wacli) mounts its receiver on the
		// shared mux and is told the loopback URL to point its subprocess at.
		if wi, ok := st.(WebhookInbound); ok {
			rt.mux.Handle(wi.Path(), wi.Handler(rt.pub))
			wi.UseCallback(rt.selfURL + wi.Path())
		}
		rt.wg.Add(1)
		go rt.supervise(ctx, kind, st)
	}
	return errors.Join(errs...)
}

// supervise runs a stream and restarts it on failure with exponential backoff (reset on
// a healthy run), isolated from everything else.
func (rt *Runtime) supervise(ctx context.Context, kind string, st Streamer) {
	defer rt.wg.Done()
	delay := rt.restartMin
	for {
		started := time.Now()
		err := st.Run(ctx, rt.pub)
		if ctx.Err() != nil || err == nil {
			return
		}
		if time.Since(started) > time.Minute {
			delay = rt.restartMin // it ran healthily for a while: reset the backoff
		}
		fmt.Printf("messenger: %s stream exited (%v), restarting in %s\n", kind, err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > rt.restartMax {
			delay = rt.restartMax
		}
	}
}

// Down cancels every stream and waits for the goroutines to exit.
func (rt *Runtime) Down() {
	rt.mu.Lock()
	cancel := rt.cancel
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	rt.wg.Wait()
}

// Send routes env to the channel named env.Channel and returns the provider-assigned
// message id (falling back to the envelope's own id).
func (rt *Runtime) Send(ctx context.Context, env envelope.Envelope) (string, error) {
	rt.mu.Lock()
	ch, ok := rt.channels[env.Channel]
	rt.mu.Unlock()
	if !ok {
		known := rt.Channels()
		names := make([]string, 0, len(known))
		for n := range known {
			names = append(names, n)
		}
		return "", fmt.Errorf("unknown channel %q (have: %s)", env.Channel, strings.Join(names, ", "))
	}
	id, err := ch.Send(ctx, env)
	if err != nil {
		return "", err
	}
	if id == "" {
		id = env.ID
	}
	return id, nil
}

// OpenSend opens just enough runtime for a one-shot send (no streams, no mux mounts).
func OpenSend(cfg *config.Config, res *SecretResolver) *Runtime {
	rt := NewRuntime(cfg.Enabled(), res, func(envelope.Envelope) {})
	for name, tcfg := range rt.seed {
		k, err := KindFor(name, tcfg)
		if err != nil {
			continue
		}
		if ch, err := k.Open(name, tcfg, rt.res); err == nil {
			rt.channels[name] = ch
		}
	}
	return rt
}
