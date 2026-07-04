package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/deemwar-products/messenger/config"
)

// Listener runs every enabled connection concurrently. It builds connections from the
// registry, brings each up idempotently via Check()||Ensure(), mounts pushed webhookers
// on one shared HTTP handler, and supervises pulled runners in their own goroutines. A
// per-connection failure is isolated: it never stops the others.
type Listener struct {
	registry *Registry
	seed     map[string]config.Transport
	pub      Publisher

	mu      sync.Mutex
	conns   map[string]Connection
	started map[string]bool
	cancels map[string]context.CancelFunc
	mux     *http.ServeMux
	wg      sync.WaitGroup

	restartDelay time.Duration
}

// NewListener builds a listener over the enabled seed. pub is where every connection
// publishes (the caller injects the inbox+webhook fan-out; tests inject a capturer).
func NewListener(reg *Registry, seed map[string]config.Transport, pub Publisher) *Listener {
	if seed == nil {
		seed = map[string]config.Transport{}
	}
	return &Listener{
		registry:     reg,
		seed:         seed,
		pub:          pub,
		conns:        map[string]Connection{},
		started:      map[string]bool{},
		cancels:      map[string]context.CancelFunc{},
		mux:          http.NewServeMux(),
		restartDelay: 500 * time.Millisecond,
	}
}

// HTTPHandler is the shared server for pushed webhook connections. It is mounted by Up
// and can be composed into the `serve` HTTP server.
func (l *Listener) HTTPHandler() http.Handler { return l.mux }

// Up brings every enabled connection up idempotently and starts it: a connection whose
// Check passes is left untouched; a down one is Ensure()d; a single failure is collected
// but does NOT stop the others. Calling Up again is a no-op for live connections.
func (l *Listener) Up(ctx context.Context) error {
	var errs []error
	for ch, t := range l.seed {
		if !t.Enabled {
			continue
		}
		if err := l.ensureAndStart(ctx, ch); err != nil {
			errs = append(errs, err) // isolated: keep going
		}
	}
	return errors.Join(errs...)
}

func (l *Listener) ensureAndStart(ctx context.Context, ch string) error {
	l.mu.Lock()
	conn, built := l.conns[ch]
	l.mu.Unlock()

	if !built {
		c, berr := l.registry.Build(ch, l.seed[ch])
		if berr != nil {
			return berr
		}
		conn = c
	}

	if conn.Check() != nil {
		if err := conn.Ensure(); err != nil {
			return fmt.Errorf("transport: ensure %q: %w", ch, err)
		}
	}

	l.mu.Lock()
	l.conns[ch] = conn
	alreadyStarted := l.started[ch]
	if !alreadyStarted {
		l.started[ch] = true
	}
	l.mu.Unlock()
	if alreadyStarted {
		return nil
	}
	return l.start(ctx, ch, conn)
}

func (l *Listener) start(ctx context.Context, ch string, conn Connection) error {
	if wh, ok := conn.(Webhooker); ok {
		l.mux.Handle(wh.Path(), wh.Handler(l.pub))
	}
	if r, ok := conn.(Runner); ok {
		cctx, cancel := context.WithCancel(ctx)
		l.mu.Lock()
		l.cancels[ch] = cancel
		l.mu.Unlock()
		l.wg.Add(1)
		go l.supervise(cctx, ch, r)
	}
	return nil
}

// supervise runs a runner and restarts it on failure via Ensure()+re-Run, isolated from
// the other connections. ctx cancellation ends supervision cleanly.
func (l *Listener) supervise(ctx context.Context, ch string, r Runner) {
	defer l.wg.Done()
	for {
		err := r.Run(ctx, l.pub)
		if ctx.Err() != nil || err == nil {
			return
		}
		_ = r.Ensure()
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.restartDelay):
		}
	}
}

// Down cancels every running connection and waits for the goroutines to exit.
func (l *Listener) Down() {
	l.mu.Lock()
	for _, cancel := range l.cancels {
		cancel()
	}
	l.cancels = map[string]context.CancelFunc{}
	l.mu.Unlock()
	l.wg.Wait()
}
