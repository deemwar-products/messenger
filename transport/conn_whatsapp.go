package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// whatsappConn shells `wacli` (a CLI wrapping whatsmeow): it pairs as a linked WhatsApp
// Web device and streams messages. This connection runs it as a long-lived subprocess,
// reads its NDJSON message stream on stdout, normalizes each line to an inbound
// Envelope, and is supervised by the Listener — a wacli crash is a re-Ensure()+restart.
//
// The subprocess argv is configurable (options["bin"], options["args"]) so a test points
// it at a fake emitter and a real deployment points it at the wacli binary.
type whatsappConn struct {
	channel string
	cfg     config.Transport

	bin  string
	args []string

	// commandContext builds the *exec.Cmd. Seam so tests inject a fake without a real
	// wacli on PATH. Defaults to exec.CommandContext.
	commandContext func(ctx context.Context, name string, arg ...string) *exec.Cmd
}

func newWhatsappConn(channel string, cfg config.Transport) (Connection, error) {
	bin := cfg.Options["bin"]
	if bin == "" {
		bin = "wacli"
	}
	var args []string
	if a := cfg.Options["args"]; a != "" {
		args = strings.Fields(a)
	} else {
		args = []string{"--json", "sync", "--follow"}
	}
	return &whatsappConn{
		channel:        channel,
		cfg:            cfg,
		bin:            bin,
		args:           args,
		commandContext: exec.CommandContext,
	}, nil
}

func (c *whatsappConn) Kind() string { return "whatsapp" }

// Check reports whether the wacli binary is resolvable. Absent binary → not ready.
func (c *whatsappConn) Check() error {
	if _, err := exec.LookPath(c.bin); err != nil {
		return fmt.Errorf("transport: whatsapp: %q not found: %w", c.bin, err)
	}
	return nil
}

// Ensure is the re-pair/re-sync hook. wacli owns its own session store; restart is the
// supervise loop re-running Run.
func (c *whatsappConn) Ensure() error { return c.Check() }

// waMessage is the slice of a wacli JSON message line we normalize.
type waMessage struct {
	ID     string `json:"id"`
	Chat   string `json:"chat"`
	Sender string `json:"sender"`
	Text   string `json:"text"`
}

// Run launches wacli and publishes an Envelope per message line until ctx is cancelled
// or the subprocess exits (the Listener restarts it on a non-nil exit).
func (c *whatsappConn) Run(ctx context.Context, pub Publisher) error {
	cmd := c.commandContext(ctx, c.bin, c.args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("transport: whatsapp: start %q: %w", c.bin, err)
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
		env := envelope.Inbound(c.channel, m.Sender, m.Text, "WhatsApp")
		env.Account = c.cfg.Account
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
		return fmt.Errorf("transport: whatsapp: wacli exited: %w", waitErr)
	}
	return nil
}
