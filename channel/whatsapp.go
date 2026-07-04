package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// WhatsApp is GLOBAL: the host has ONE paired device (wacli's linked WhatsApp-Web
// session), and every configured whatsapp channel is a named GROUP on that device.
// Exactly one `wacli --json sync --follow` stream runs no matter how many whatsapp
// channels exist; inbound is routed to the channel whose options["group"] JID matches
// the message's chat, falling back to the catch-all channel (one configured with no
// group), else the first channel by name. Sends target the channel's group (or an
// explicit --to thread), quoting via wacli --quote when ReplyTo is set.

// whatsappChannel is one named group (or the catch-all) on the shared device.
type whatsappChannel struct {
	name string
	cfg  config.Transport
}

func openWhatsapp(name string, cfg config.Transport, _ *SecretResolver) (Channel, error) {
	return &whatsappChannel{name: name, cfg: cfg}, nil
}

func (c *whatsappChannel) Name() string { return c.name }
func (c *whatsappChannel) Kind() string { return "whatsapp" }

// Send shells `wacli send text`. Target = explicit ThreadID, else the channel's group.
// The wacli send id is returned when its JSON output carries one.
func (c *whatsappChannel) Send(ctx context.Context, env envelope.Envelope) (string, error) {
	bin := waBin(c.cfg)
	to := env.ThreadID
	if to == "" {
		to = c.cfg.Options["group"]
	}
	if to == "" {
		return "", fmt.Errorf("channel: whatsapp %q: no target (pass --to or configure --group <jid>)", c.name)
	}
	args := []string{"--json", "send", "text", "--to", to, "--message", env.Text}
	if env.ReplyTo != "" {
		args = append(args, "--quote", env.ReplyTo)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("channel: whatsapp: wacli send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Best-effort: surface the id wacli assigned so the caller can thread onto it.
	var res struct {
		Data struct {
			ID        string `json:"id"`
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if json.Unmarshal(out, &res) == nil {
		if res.Data.ID != "" {
			return res.Data.ID, nil
		}
		if res.Data.MessageID != "" {
			return res.Data.MessageID, nil
		}
	}
	return "", nil
}

// whatsappStream is the ONE shared inbound stream for every whatsapp channel: it runs
// wacli as a long-lived subprocess, reads its NDJSON message lines, routes each by chat
// JID to the matching group channel, and publishes the normalized Envelope. Supervised
// by the runtime (crash = backoff + restart).
type whatsappStream struct {
	bin  string
	args []string

	byGroup  map[string]string // group jid -> channel name
	catchAll string            // channel with no group (else first by name)
	accounts map[string]string // channel name -> account label

	// commandContext is the exec seam so tests inject a fake emitter.
	commandContext func(ctx context.Context, name string, arg ...string) *exec.Cmd
}

func openWhatsappStream(chans map[string]config.Transport, _ *SecretResolver) (Streamer, error) {
	if len(chans) == 0 {
		return nil, fmt.Errorf("channel: whatsapp stream: no channels")
	}
	names := make([]string, 0, len(chans))
	for n := range chans {
		names = append(names, n)
	}
	sort.Strings(names)

	s := &whatsappStream{
		byGroup:        map[string]string{},
		accounts:       map[string]string{},
		commandContext: exec.CommandContext,
	}
	for _, n := range names {
		cfg := chans[n]
		if g := cfg.Options["group"]; g != "" {
			s.byGroup[g] = n
		} else if s.catchAll == "" {
			s.catchAll = n
		}
		s.accounts[n] = cfg.Account
		// bin/args overrides: first channel that sets them wins (they describe the ONE
		// device, not a channel).
		if s.bin == "" {
			s.bin = cfg.Options["bin"]
		}
		if len(s.args) == 0 && cfg.Options["args"] != "" {
			s.args = strings.Fields(cfg.Options["args"])
		}
	}
	if s.catchAll == "" {
		s.catchAll = names[0]
	}
	if s.bin == "" {
		s.bin = "wacli"
	}
	if len(s.args) == 0 {
		s.args = []string{"--json", "sync", "--follow"}
	}
	return s, nil
}

// waMessage is the slice of a wacli JSON message line we normalize.
type waMessage struct {
	ID     string `json:"id"`
	Chat   string `json:"chat"`
	Sender string `json:"sender"`
	Text   string `json:"text"`
}

// route returns the channel name a message in chat belongs to.
func (s *whatsappStream) route(chat string) string {
	if n, ok := s.byGroup[chat]; ok {
		return n
	}
	return s.catchAll
}

func (s *whatsappStream) Run(ctx context.Context, pub Publisher) error {
	cmd := s.commandContext(ctx, s.bin, s.args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("channel: whatsapp: start %q: %w", s.bin, err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] != '{' {
			continue // skip non-JSON progress lines
		}
		var m waMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil || m.Text == "" {
			continue
		}
		name := s.route(m.Chat)
		env := envelope.Inbound(name, m.Sender, m.Text, "WhatsApp")
		env.Account = s.accounts[name]
		if m.ID != "" {
			env.ID = m.ID // wacli's stable message id → reply/dedupe key
		}
		if m.Chat != "" {
			env.ThreadID = m.Chat
		}
		pub(env)
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil // cancelled: clean stop
	}
	if waitErr != nil {
		return fmt.Errorf("channel: whatsapp: wacli exited: %w", waitErr)
	}
	return nil
}

func waBin(cfg config.Transport) string {
	if b := cfg.Options["bin"]; b != "" {
		return b
	}
	return "wacli"
}

// DeviceStatus is the host's global WhatsApp pair state, read from `wacli doctor`.
type DeviceStatus struct {
	Installed     bool
	Authenticated bool
	LinkedJID     string
}

// WhatsappDeviceStatus probes the ONE global device: is wacli installed, is the host
// paired, and as which JID. Used by the CLI wizard so `channel add whatsapp` /
// `channel connect` never re-pair an already-linked device.
func WhatsappDeviceStatus(ctx context.Context, bin string) DeviceStatus {
	if bin == "" {
		bin = "wacli"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return DeviceStatus{}
	}
	out, err := exec.CommandContext(ctx, bin, "doctor", "--json").Output()
	if err != nil {
		return DeviceStatus{Installed: true}
	}
	var d struct {
		Data struct {
			Authenticated bool   `json:"authenticated"`
			LinkedJID     string `json:"linked_jid"`
		} `json:"data"`
	}
	if json.Unmarshal(out, &d) != nil {
		return DeviceStatus{Installed: true}
	}
	return DeviceStatus{Installed: true, Authenticated: d.Data.Authenticated, LinkedJID: d.Data.LinkedJID}
}
