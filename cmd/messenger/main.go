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
	"os/exec"
	"os/signal"
	"sort"
	"strings"
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
	case "channel", "channels":
		err = cmdChannel(os.Args[2:])
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
  messenger setup                       scaffold home + an empty config
  messenger channel add <kind> <name>   add a channel (kind: telegram|whatsapp|hook)
  messenger channel list                list configured channels
  messenger channel remove <name>       remove a channel
  messenger channel connect <name>      connect/pair a channel (wacli auth / telegram setWebhook)
  messenger listen [--addr :14310] [--webhook URL]
  messenger send   --channel <name> --text "hi" [--to THREAD] [--reply-to MSGID]
  messenger serve  [--addr :14310]

channel add flags:
  --token-env NAME    env var holding the token (telegram bot token, hook secret)
  --chat-id ID        default target chat/channel id (telegram: the channel to post to)
  --account NAME      platform account/workspace label
  --token-vault NAME  age vault entry holding the token (instead of --token-env)
  --option k=v        repeatable free-form option (e.g. --option bin=wacli)
  --disabled          add the channel disabled

  e.g.  messenger channel add whatsapp home
        messenger channel add telegram mybot --token-env TELEGRAM_BOT_TOKEN --chat-id -1001234567890
`)
}

// optionFlags collects repeatable --option k=v pairs.
type optionFlags map[string]string

func (o optionFlags) String() string { return "" }
func (o optionFlags) Set(v string) error {
	i := strings.IndexByte(v, '=')
	if i < 0 {
		return fmt.Errorf("option must be k=v, got %q", v)
	}
	o[v[:i]] = v[i+1:]
	return nil
}

// cmdChannel is the channel management group: add / list / remove / connect. All edits
// write config.toml; secrets are referenced by NAME only (--token-env / --token-vault),
// never a value.
func cmdChannel(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: messenger channel <add|list|remove|connect> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return channelList(rest)
	case "add":
		return channelAdd(rest)
	case "remove", "rm":
		return channelRemove(rest)
	case "connect":
		return channelConnect(rest)
	default:
		return fmt.Errorf("unknown channel subcommand %q", sub)
	}
}

func channelList(args []string) error {
	fs := flag.NewFlagSet("channel list", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.Transports) == 0 {
		fmt.Println("no channels configured. add one: messenger channel add <kind> <name>")
		return nil
	}
	names := make([]string, 0, len(cfg.Transports))
	for n := range cfg.Transports {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("%-16s %-10s %-8s %s\n", "NAME", "KIND", "ENABLED", "TARGET/OPTIONS")
	for _, n := range names {
		t := cfg.Transports[n]
		kind := t.Kind
		if kind == "" {
			kind = n
		}
		detail := ""
		if id := t.Options["chatId"]; id != "" {
			detail = "chat=" + id
		}
		fmt.Printf("%-16s %-10s %-8v %s\n", n, kind, t.Enabled, detail)
	}
	return nil
}

func channelAdd(args []string) error {
	// Leading positionals first (<kind> <name>), then flags — Go's flag pkg stops at the
	// first non-flag token, so we split them ourselves.
	if len(args) < 2 {
		return fmt.Errorf("usage: messenger channel add <kind> <name> [flags]")
	}
	kind, name := args[0], args[1]
	fs := flag.NewFlagSet("channel add", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	tokenEnv := fs.String("token-env", "", "env var NAME holding the token")
	tokenVault := fs.String("token-vault", "", "age vault entry NAME holding the token")
	account := fs.String("account", "", "platform account/workspace label")
	chatID := fs.String("chat-id", "", "default target chat/channel id")
	disabled := fs.Bool("disabled", false, "add the channel disabled")
	opts := optionFlags{}
	fs.Var(&opts, "option", "repeatable free-form option k=v")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	switch kind {
	case "telegram", "whatsapp", "hook":
	default:
		return fmt.Errorf("unknown kind %q (telegram|whatsapp|hook)", kind)
	}

	path := *cfgPath
	if path == "" {
		path = home.ConfigPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		cfg = &config.Config{Transports: map[string]config.Transport{}}
	}
	if cfg.Transports == nil {
		cfg.Transports = map[string]config.Transport{}
	}
	if _, exists := cfg.Transports[name]; exists {
		return fmt.Errorf("channel %q already exists (remove it first)", name)
	}
	if *chatID != "" {
		opts["chatId"] = *chatID
	}
	if len(opts) == 0 {
		opts = nil
	}
	cfg.Transports[name] = config.Transport{
		Enabled:    !*disabled,
		Kind:       kind,
		Account:    *account,
		TokenEnv:   *tokenEnv,
		TokenVault: *tokenVault,
		Options:    opts,
	}
	if err := os.MkdirAll(home.Dir(), 0o700); err != nil {
		return err
	}
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("added channel %q (kind=%s, enabled=%v) to %s\n", name, kind, !*disabled, path)
	if *tokenEnv != "" {
		fmt.Printf("  remember to export %s (value never printed)\n", *tokenEnv)
	}
	if kind == "whatsapp" {
		fmt.Printf("  pair once: messenger channel connect %s\n", name)
	}
	return nil
}

func channelRemove(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: messenger channel remove <name>")
	}
	name := args[0]
	fs := flag.NewFlagSet("channel remove", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	path := *cfgPath
	if path == "" {
		path = home.ConfigPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if _, ok := cfg.Transports[name]; !ok {
		return fmt.Errorf("no channel named %q", name)
	}
	delete(cfg.Transports, name)
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("removed channel %q\n", name)
	return nil
}

// channelConnect performs the connect/pair action for a channel: whatsapp runs the
// paired-device auth (wacli auth); telegram prints the setWebhook to run against a public
// URL; hook needs no connect. It never handles a secret value directly.
func channelConnect(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: messenger channel connect <name>")
	}
	name := args[0]
	fs := flag.NewFlagSet("channel connect", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	publicURL := fs.String("public-url", "", "public base URL (telegram setWebhook)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	t, ok := cfg.Transports[name]
	if !ok {
		return fmt.Errorf("no channel named %q", name)
	}
	kind := t.Kind
	if kind == "" {
		kind = name
	}
	switch kind {
	case "whatsapp":
		bin := t.Options["bin"]
		if bin == "" {
			bin = "wacli"
		}
		fmt.Printf("pairing whatsapp %q via %s auth — scan the QR:\n", name, bin)
		cmd := exec.Command(bin, "auth")
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		return cmd.Run()
	case "telegram":
		path := t.Options["path"]
		if path == "" {
			path = "/telegram/" + name
		}
		if *publicURL == "" {
			fmt.Printf("telegram %q webhook path is %s\n", name, path)
			fmt.Printf("re-run with --public-url https://<host> to print the setWebhook call\n")
			return nil
		}
		fmt.Printf("set the webhook (run against your bot token, kept out of this output):\n")
		fmt.Printf("  curl -sS \"https://api.telegram.org/bot$%s/setWebhook\" -d \"url=%s%s\"\n",
			envNameOr(t.TokenEnv, "TELEGRAM_BOT_TOKEN"), strings.TrimRight(*publicURL, "/"), path)
		return nil
	default:
		fmt.Printf("channel %q (kind=%s) needs no connect step\n", name, kind)
		return nil
	}
}

func envNameOr(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

// cmdSetup scaffolds the home directory and an empty config.toml (idempotent unless
// --force), then points at `channel add`. Channels are added individually so many of any
// kind can coexist.
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
		fmt.Printf("config already exists at %s (use --force to reset)\n", path)
	} else {
		empty := &config.Config{ServeTokenEnv: "MESSENGER_SERVE_TOKEN", Transports: map[string]config.Transport{}}
		if err := config.Save(path, empty); err != nil {
			return err
		}
		fmt.Printf("scaffolded home + empty config at %s\n", path)
	}
	fmt.Print(`
add channels (many of any kind can coexist):
  messenger channel add whatsapp home
  messenger channel add telegram mybot --token-env TELEGRAM_BOT_TOKEN --chat-id -1001234567890
  messenger channel add hook incoming --token-env MESSENGER_HOOK_SECRET
then:
  messenger channel list
  messenger channel connect <name>     # whatsapp QR pair / telegram setWebhook
  messenger serve                      # channel webhooks + POST /send, GET /inbox, /health
secrets are referenced by NAME only — export the values, never printed here.
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
