// Command messenger is the standalone, broker-free channel I/O product: one static
// binary with three verbs.
//
//	messenger listen   run the enabled channel transports; append each inbound message to
//	                   the inbox and optionally POST it to a subscriber webhook. Pushed
//	                   channels (telegram/hook) are served on --addr.
//	messenger send     one-shot egress: deliver a message on a channel, optionally
//	                   threading a reply (--to <thread> --reply-to <message id>).
//	messenger serve    the small HTTP server: channel webhooks + the consumer API
//	                   (POST /send, GET /inbox?since=N, GET /health) on one --addr.
//
// Config lives at $MESSENGER_HOME/config.toml (or ~/.config/messenger/config.toml).
// Secrets are referenced by NAME only and resolved host-only at the point of use.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/home"
	"github.com/deemwar-products/messenger/inbox"
	"github.com/deemwar-products/messenger/server"
	"github.com/deemwar-products/messenger/transport"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "setup":
		err = cmdSetup(os.Args[2:])
	case "listen":
		err = cmdListen(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "messenger: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "messenger:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `messenger — standalone channel I/O (telegram, whatsapp, hook)

usage:
  messenger setup  [--config PATH] [--force]
  messenger listen [--config PATH] [--addr :14310] [--webhook URL]
  messenger send   --channel telegram --text "hi" [--to THREAD] [--reply-to MSGID] [--config PATH]
  messenger serve  [--config PATH] [--addr :14310]
`)
}

// starterConfig is the commented config.toml `setup` writes. Secrets are referenced by
// NAME only — never a value.
const starterConfig = `# messenger config — secrets are referenced by NAME only, never a value.
# The HTTP bearer token for POST /send / GET /inbox (leave empty for loopback dev):
serveTokenEnv = "MESSENGER_SERVE_TOKEN"

[transports.telegram]
enabled  = true
kind     = "telegram"
tokenEnv = "TELEGRAM_BOT_TOKEN"   # bot token (used only by send)
# options.path        = "/telegram/telegram"   # webhook mount path
# options.secretHeader = "X-Telegram-Bot-Api-Secret-Token"  # verify inbound webhook

[transports.whatsapp]
enabled = false
kind    = "whatsapp"
# options.bin  = "wacli"                       # the paired-device CLI to shell
# options.args = "--json sync --follow"        # NDJSON message stream

[transports.hook]
enabled  = true
kind     = "hook"
tokenEnv = "MESSENGER_HOOK_SECRET"   # HMAC shared secret for inbound POST /hook/hook
`

// cmdSetup scaffolds the home directory and a starter config.toml (idempotent unless
// --force). It prints, but never writes, the secret NAMES to set.
func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path (default $MESSENGER_HOME/config.toml)")
	force := fs.Bool("force", false, "overwrite an existing config")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *cfgPath
	if path == "" {
		path = home.ConfigPath()
	}
	if err := os.MkdirAll(home.Dir(), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !*force {
		fmt.Printf("config already exists at %s (use --force to overwrite)\n", path)
	} else {
		if err := os.WriteFile(path, []byte(starterConfig), 0o600); err != nil {
			return err
		}
		fmt.Printf("wrote starter config to %s\n", path)
	}
	fmt.Print(`
next steps:
  1. export the secret NAMES referenced in the config (values, never printed):
       export TELEGRAM_BOT_TOKEN=...        # your bot token
       export MESSENGER_HOOK_SECRET=...     # any strong shared secret
       export MESSENGER_SERVE_TOKEN=...     # bearer for the HTTP API (optional)
  2. messenger serve            # channel webhooks + POST /send, GET /inbox, /health
  3. point Telegram's setWebhook at  https://<public-host>/telegram/telegram
  4. (whatsapp) pair once with `+"`wacli auth`"+`, then enable [transports.whatsapp]
`)
	return nil
}

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		path = home.ConfigPath()
	}
	return config.Load(path)
}

// cmdListen runs the enabled transports, appending inbound to the inbox and (optionally)
// POSTing each envelope to a subscriber webhook. Pushed channels are served on --addr.
func cmdListen(args []string) error {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path (default $MESSENGER_HOME/config.toml)")
	addr := fs.String("addr", ":14310", "address for pushed channel webhooks")
	webhook := fs.String("webhook", "", "optional subscriber URL to POST each inbound envelope to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	box, err := inbox.Open(home.InboxPath())
	if err != nil {
		return err
	}
	pub := fanout(box, *webhook)
	lst := transport.NewListener(transport.DefaultRegistry(), cfg.Enabled(), pub)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := lst.Up(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "messenger: listen (partial):", err)
	}
	srv := &http.Server{Addr: *addr, Handler: lst.HTTPHandler()}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()); lst.Down() }()
	fmt.Printf("messenger listen on %s (channels: %v)\n", *addr, keys(cfg.Enabled()))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// cmdSend delivers one message, threading a reply when --reply-to is set.
func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	channel := fs.String("channel", "", "channel to send on (telegram|whatsapp|hook)")
	text := fs.String("text", "", "message text")
	to := fs.String("to", "", "thread/chat id to deliver to")
	replyTo := fs.String("reply-to", "", "message id this send replies to (threads the reply)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *channel == "" || *text == "" {
		return fmt.Errorf("--channel and --text are required")
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	env := envelope.Normalize(envelope.Envelope{
		Channel:  *channel,
		Text:     *text,
		ThreadID: *to,
		ReplyTo:  *replyTo,
		Origin:   "messenger",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Deliver(ctx, cfg, transport.DefaultSenderRegistry(), transport.NewSecretResolver(nil), env); err != nil {
		return err
	}
	fmt.Printf("sent id=%s channel=%s\n", env.ID, env.Channel)
	return nil
}

// cmdServe runs the channel webhooks + the consumer API on one port.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	addr := fs.String("addr", ":14310", "address to serve on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	box, err := inbox.Open(home.InboxPath())
	if err != nil {
		return err
	}
	resolver := transport.NewSecretResolver(nil)
	lst := transport.NewListener(transport.DefaultRegistry(), cfg.Enabled(), fanout(box, ""))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := lst.Up(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "messenger: serve (partial):", err)
	}

	// Resolve the bearer token by NAME once, host-only; the value never enters a log.
	token := ""
	if cfg.ServeTokenEnv != "" {
		token = os.Getenv(cfg.ServeTokenEnv)
	}
	srv := server.New(cfg, box, transport.DefaultSenderRegistry(), resolver, token, lst.HTTPHandler())
	hs := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() { <-ctx.Done(); _ = hs.Shutdown(context.Background()); lst.Down() }()
	fmt.Printf("messenger serve on %s (channels: %v, auth: %v)\n", *addr, keys(cfg.Enabled()), token != "")
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// fanout builds the Publisher listen/serve inject: append to the inbox, then (if set)
// POST the envelope to a subscriber webhook. A webhook failure is logged, never fatal.
func fanout(box *inbox.Inbox, webhook string) transport.Publisher {
	client := &http.Client{Timeout: 10 * time.Second}
	return func(env envelope.Envelope) {
		if err := box.Append(env); err != nil {
			fmt.Fprintln(os.Stderr, "messenger: inbox append:", err)
		}
		if webhook == "" {
			return
		}
		body, _ := json.Marshal(env)
		req, err := http.NewRequest(http.MethodPost, webhook, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "messenger: webhook push:", err)
			return
		}
		_ = resp.Body.Close()
	}
}

func keys(m map[string]config.Transport) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
