// Package subscription is messenger's durable consumer delivery: each named
// subscription gets every inbound envelope (optionally filtered to channels) POSTed to
// its URL IN ORDER, with its own on-disk cursor advanced only on success — at-least-once
// delivery, and a consumer that was down catches up from where it left off. The inbox
// (append-only NDJSON) is the queue; the cursor is a 1-based line offset per consumer.
//
// A push is HMAC-signed (X-Messenger-Signature-256, sha256=<hex> over the body) when the
// subscription names a secret env var. The secret is resolved by NAME at the point of
// the request; its value never enters config, a log, or the envelope.
package subscription

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deemwar-products/messenger/channel"
	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/inbox"
)

// Dispatcher runs one delivery loop per enabled subscription over the shared inbox.
type Dispatcher struct {
	box       *inbox.Inbox
	cursorDir string
	subs      map[string]config.Subscription
	client    *http.Client

	notify chan struct{}
	wg     sync.WaitGroup

	tick       time.Duration
	backoffMin time.Duration
	backoffMax time.Duration
}

// New builds a dispatcher over the enabled subscriptions. cursorDir holds one cursor
// file per subscription ($MESSENGER_HOME/cursors/<name>).
func New(box *inbox.Inbox, cursorDir string, subs map[string]config.Subscription) *Dispatcher {
	enabled := map[string]config.Subscription{}
	for n, s := range subs {
		if s.Enabled && s.URL != "" {
			enabled[n] = s
		}
	}
	return &Dispatcher{
		box:        box,
		cursorDir:  cursorDir,
		subs:       enabled,
		client:     &http.Client{Timeout: 10 * time.Second},
		notify:     make(chan struct{}, 1),
		tick:       5 * time.Second,
		backoffMin: time.Second,
		backoffMax: time.Minute,
	}
}

// Count reports how many subscriptions are being served.
func (d *Dispatcher) Count() int { return len(d.subs) }

// Notify wakes the delivery loops (called by the publisher after an inbox append).
// Non-blocking; coalesces.
func (d *Dispatcher) Notify() {
	select {
	case d.notify <- struct{}{}:
	default:
	}
}

// Run starts one loop per subscription and blocks until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	if len(d.subs) == 0 {
		<-ctx.Done()
		return
	}
	// Fan the single notify out to per-subscription wakeups.
	wake := make([]chan struct{}, 0, len(d.subs))
	for name, sub := range d.subs {
		w := make(chan struct{}, 1)
		wake = append(wake, w)
		d.wg.Add(1)
		go d.loop(ctx, name, sub, w)
	}
	for {
		select {
		case <-ctx.Done():
			d.wg.Wait()
			return
		case <-d.notify:
			for _, w := range wake {
				select {
				case w <- struct{}{}:
				default:
				}
			}
		}
	}
}

// loop is one subscription's delivery loop: drain from the cursor, then sleep until a
// wake or the tick; back off on push failure and retry the same message.
func (d *Dispatcher) loop(ctx context.Context, name string, sub config.Subscription, wake chan struct{}) {
	defer d.wg.Done()
	delay := d.backoffMin
	for {
		delivered, err := d.drain(ctx, name, sub)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay *= 2
			if delay > d.backoffMax {
				delay = d.backoffMax
			}
			continue
		}
		if delivered > 0 {
			delay = d.backoffMin
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-time.After(d.tick):
		}
	}
}

// drain pushes everything after the cursor in order, persisting the cursor after each
// success. Returns how many were delivered; an error means a push failed (cursor points
// at the failed message, so it is retried).
func (d *Dispatcher) drain(ctx context.Context, name string, sub config.Subscription) (int, error) {
	cur := d.readCursor(name)
	msgs, _, err := d.box.Since(cur)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for i, env := range msgs {
		if len(sub.Channels) > 0 && !contains(sub.Channels, env.Channel) {
			// Filtered out still advances the cursor: it is consumed for this consumer.
			if err := d.writeCursor(name, cur+i+1); err != nil {
				return delivered, err
			}
			continue
		}
		if err := d.push(ctx, sub, env); err != nil {
			return delivered, err
		}
		if err := d.writeCursor(name, cur+i+1); err != nil {
			return delivered, err
		}
		delivered++
	}
	return delivered, nil
}

// push POSTs one envelope, HMAC-signed when the subscription names a secret env var.
func (d *Dispatcher) push(ctx context.Context, sub config.Subscription, env any) error {
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if sub.SecretEnv != "" {
		secret := os.Getenv(sub.SecretEnv)
		if secret == "" {
			return fmt.Errorf("subscription: secret env %q is unset", sub.SecretEnv)
		}
		req.Header.Set("X-Messenger-Signature-256", channel.SignHMAC([]byte(secret), body))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("subscription: push status %d", resp.StatusCode)
	}
	return nil
}

func (d *Dispatcher) cursorPath(name string) string {
	return filepath.Join(d.cursorDir, name)
}

func (d *Dispatcher) readCursor(name string) int {
	b, err := os.ReadFile(d.cursorPath(name))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (d *Dispatcher) writeCursor(name string, n int) error {
	if err := os.MkdirAll(d.cursorDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(d.cursorPath(name), []byte(strconv.Itoa(n)), 0o600)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
