// Command messenger is the standalone, broker-free conversation hub: one static binary,
// the CLI is the whole interface.
//
//	messenger setup      scaffold home + an empty config, then guide channel adds
//	messenger status     one-glance health: config, channels, whatsapp device, inbox, subs
//	messenger channel    add / list / remove / connect (wizard-grade; whatsapp device is global)
//	messenger subscribe  add / list / remove durable consumer push subscriptions
//	messenger listen     ingress + subscription dispatch (no consumer API)
//	messenger send       one-shot egress; prints the provider-assigned message id
//	messenger inject     sign + POST a message INTO the running hub via a webhook channel
//	messenger serve      everything on one port: channel webhooks + POST /send,
//	                     GET /inbox?since=N, GET /health + the subscription dispatcher
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
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/deemwar-products/messenger/channel"
	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/home"
	"github.com/deemwar-products/messenger/inbox"
	"github.com/deemwar-products/messenger/server"
	"github.com/deemwar-products/messenger/skills"
	"github.com/deemwar-products/messenger/subscription"
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
	case "status":
		err = cmdStatus(os.Args[2:])
	case "channel", "channels":
		err = cmdChannel(os.Args[2:])
	case "subscribe", "subscription", "subscriptions":
		err = cmdSubscribe(os.Args[2:])
	case "listen":
		err = cmdListen(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "inject":
		err = cmdInject(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "install":
		err = cmdInstall(os.Args[2:])
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
	fmt.Fprint(os.Stderr, `messenger — broker-free conversation hub (telegram, whatsapp, webhook)

usage:
  messenger setup                          scaffold home + an empty config, then guide
  messenger status                         config + channels + whatsapp device + inbox + subs
  messenger channel add telegram <name> --token-env NAME [--chat-id ID]
  messenger channel add whatsapp <name> [--group <group-jid>]     (device is global)
  messenger channel add webhook  <name> --token-env NAME
  messenger channel list | remove <name> | connect <name> [--public-url URL]
  messenger channel test [<name>]          probe connectivity (whatsapp device, telegram getMe, webhook secret)
  messenger install --skills               install the embedded agent skill into ~/.claude/skills
  messenger subscribe add <name> --url URL [--channels a,b] [--secret-env NAME]
  messenger subscribe list | remove <name>
  messenger listen [--addr :14310] [--webhook URL]
  messenger send   --channel <name> [--text "hi"] [--file PATH|URL] [--to THREAD] [--reply-to MSGID]
                                           (--text and/or --file; --file attaches a local path or http(s) URL)
  messenger inject --channel <webhook-name> --text "MSG" [--sender NAME] [--thread ID] [--reply-to MSGID]
                                           (scripted ingress: signs + POSTs to the running hub's /webhook/<name>)
  messenger serve  [--addr :14310]

kinds:
  telegram  many channels, each its OWN bot (own --token-env) + default --chat-id
  whatsapp  ONE global paired device; each channel = a GROUP (--group <jid>);
            one channel with no --group is the catch-all for unmatched chats
  webhook   many channels, each its own HMAC-signed path (/webhook/<name>) + secret

secrets are referenced by NAME only (--token-env / --token-vault) — never a value.
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

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		path = home.ConfigPath()
	}
	return config.Load(path)
}

// loadOrInitConfig loads the config, scaffolding an empty one when absent.
func loadOrInitConfig(path string) (*config.Config, string, error) {
	if path == "" {
		path = home.ConfigPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, path, err
		}
		cfg = &config.Config{
			ServeTokenEnv: "MESSENGER_SERVE_TOKEN",
			Transports:    map[string]config.Transport{},
			Subscriptions: map[string]config.Subscription{},
		}
	}
	return cfg, path, nil
}

func saveConfig(path string, cfg *config.Config) error {
	if err := os.MkdirAll(home.Dir(), 0o700); err != nil {
		return err
	}
	return config.Save(path, cfg)
}

// --- setup / status ---------------------------------------------------------------------

// cmdSetup scaffolds the home directory and an empty config.toml (idempotent unless
// --force), then guides the next steps — including the global whatsapp device state.
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
	printWhatsappHint()
	fmt.Print(`
next — add channels (many of any kind, keyed by name):
  messenger channel add whatsapp ops --group 123456789@g.us
  messenger channel add telegram mybot --token-env TELEGRAM_BOT_TOKEN --chat-id -1001234567890
  messenger channel add webhook incoming --token-env MESSENGER_HOOK_SECRET
then:
  messenger channel list
  messenger channel connect <name>     # whatsapp QR pair (once per host) / telegram setWebhook
  messenger channel test               # probe every channel without sending
  messenger subscribe add factory --url http://localhost:9000/hook
  messenger serve                      # webhooks + /send + /inbox + subscription push
  messenger install --skills           # let AI agents drive messenger (~/.claude/skills)
secrets are referenced by NAME only — export the values, never printed here.
`)
	return nil
}

// printWhatsappHint reports the GLOBAL device state so the user knows whether pairing
// is even needed.
func printWhatsappHint() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := channel.WhatsappDeviceStatus(ctx, "")
	switch {
	case !st.Installed:
		fmt.Println("whatsapp: wacli not found on PATH — install it to use whatsapp channels")
	case st.Authenticated:
		fmt.Printf("whatsapp: device already linked (%s) — no pairing needed, just add group channels\n", st.LinkedJID)
	default:
		fmt.Println("whatsapp: wacli installed but not paired — `messenger channel connect <name>` will run the QR pair once")
	}
}

// cmdStatus is the one-glance health view: config path, channels, the global whatsapp
// device, inbox size, and subscriptions with their cursors.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *cfgPath
	if path == "" {
		path = home.ConfigPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("no config at %s — run: messenger setup\n", path)
			return nil
		}
		return err
	}
	fmt.Printf("config: %s\n", path)
	if probeRunning(":14310") {
		fmt.Println("server: RUNNING on :14310 (reuse it — do not start a second instance)")
	} else {
		fmt.Println("server: not running on :14310 (start: messenger serve)")
	}
	fmt.Printf("channels: %d configured\n", len(cfg.Transports))
	_ = channelListPrint(cfg)
	printWhatsappHint()

	if msgs, next, err := (&inboxAt{home.InboxPath()}).count(); err == nil {
		fmt.Printf("inbox: %d messages (%s)\n", next, home.InboxPath())
		_ = msgs
	}
	if len(cfg.Subscriptions) > 0 {
		fmt.Printf("subscriptions:\n")
		names := sortedKeys(cfg.Subscriptions)
		for _, n := range names {
			s := cfg.Subscriptions[n]
			cur := readCursorFile(home.Path("cursors", n))
			fmt.Printf("  %-16s enabled=%-5v cursor=%-6d url=%s channels=%s\n",
				n, s.Enabled, cur, s.URL, strings.Join(s.Channels, ","))
		}
	}
	return nil
}

type inboxAt struct{ path string }

func (i *inboxAt) count() (int, int, error) {
	box, err := inbox.Open(i.path)
	if err != nil {
		return 0, 0, err
	}
	msgs, next, err := box.Since(0)
	return len(msgs), next, err
}

func readCursorFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &n)
	return n
}

// --- channel management -------------------------------------------------------------------

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
	case "test":
		return channelTest(rest)
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
	return channelListPrint(cfg)
}

func channelListPrint(cfg *config.Config) error {
	if len(cfg.Transports) == 0 {
		fmt.Println("no channels configured. add one: messenger channel add <kind> <name>")
		return nil
	}
	names := sortedKeys(cfg.Transports)
	fmt.Printf("%-16s %-10s %-8s %s\n", "NAME", "KIND", "ENABLED", "TARGET")
	for _, n := range names {
		t := cfg.Transports[n]
		kind := channel.NormalizeKind(t.Kind, n)
		detail := ""
		switch {
		case t.Options["group"] != "":
			detail = "group=" + t.Options["group"]
		case t.Options["chatId"] != "":
			detail = "chat=" + t.Options["chatId"]
		case kind == "whatsapp":
			detail = "(catch-all: unmatched chats land here)"
		case kind == "webhook":
			p := t.Options["path"]
			if p == "" {
				p = "/webhook/" + n
			}
			detail = "path=" + p
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
	kind, name := channel.NormalizeKind(args[0], args[0]), args[1]
	fs := flag.NewFlagSet("channel add", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	tokenEnv := fs.String("token-env", "", "env var NAME holding the token/secret")
	tokenVault := fs.String("token-vault", "", "age vault entry NAME holding the token")
	account := fs.String("account", "", "platform account/workspace label")
	chatID := fs.String("chat-id", "", "telegram: default target chat/channel id")
	group := fs.String("group", "", "whatsapp: the group JID this channel is bound to")
	disabled := fs.Bool("disabled", false, "add the channel disabled")
	opts := optionFlags{}
	fs.Var(&opts, "option", "repeatable free-form option k=v")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	spec, ok := channel.Kinds()[kind]
	if !ok {
		return fmt.Errorf("unknown kind %q (%s)", args[0], strings.Join(channel.KindNames(), "|"))
	}
	if spec.RequiresToken && *tokenEnv == "" && *tokenVault == "" {
		return fmt.Errorf("%s channels need a secret: pass --token-env NAME (or --token-vault)", kind)
	}

	cfg, path, err := loadOrInitConfig(*cfgPath)
	if err != nil {
		return err
	}
	if _, exists := cfg.Transports[name]; exists {
		return fmt.Errorf("channel %q already exists (remove it first)", name)
	}
	if *chatID != "" {
		opts["chatId"] = *chatID
	}
	if *group != "" {
		opts["group"] = *group
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
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	fmt.Printf("added channel %q (kind=%s, enabled=%v) to %s\n", name, kind, !*disabled, path)
	if *tokenEnv != "" {
		fmt.Printf("  remember to export %s (value never printed)\n", *tokenEnv)
	}
	switch kind {
	case "whatsapp":
		// The device is GLOBAL — say whether pairing is even needed, and nudge toward
		// a group binding when none was given.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st := channel.WhatsappDeviceStatus(ctx, waBinOf(cfg.Transports[name]))
		switch {
		case !st.Installed:
			fmt.Println("  wacli not found on PATH — install it, then: messenger channel connect " + name)
		case st.Authenticated:
			fmt.Printf("  device already linked (%s) — no pairing needed\n", st.LinkedJID)
		default:
			fmt.Printf("  pair the device once (serves ALL whatsapp channels): messenger channel connect %s\n", name)
		}
		if *group == "" {
			fmt.Println("  no --group set: this channel is the catch-all. bind a group with:")
			fmt.Printf("    messenger channel connect %s     # lists your groups + their JIDs\n", name)
		}
	case "telegram":
		fmt.Printf("  register the webhook: messenger channel connect %s --public-url https://<host>\n", name)
	case "webhook":
		p := opts["path"]
		if p == "" {
			p = "/webhook/" + name
		}
		fmt.Printf("  inbound path: %s (HMAC X-Hub-Signature-256 with $%s)\n", p, *tokenEnv)
	}
	return nil
}

func waBinOf(t config.Transport) string {
	return t.Options["bin"] // "" lets the probe default to wacli
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

// channelConnect performs the connect/pair action for a channel. whatsapp is GLOBAL:
// if the device is already linked it reports the JID and lists the groups (so the user
// can bind --group JIDs) instead of re-pairing; only an unlinked device runs `wacli
// auth` (QR) — once, for all whatsapp channels. telegram prints the setWebhook to run
// against a public URL; webhook prints a signed-call example. Never handles a secret value.
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
	kind := channel.NormalizeKind(t.Kind, name)
	switch kind {
	case "whatsapp":
		return connectWhatsapp(name, t)
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
	case "webhook":
		p := t.Options["path"]
		if p == "" {
			p = "/webhook/" + name
		}
		fmt.Printf("webhook %q needs no pairing. callers POST to %s signed with $%s:\n", name, p, envNameOr(t.TokenEnv, "MESSENGER_HOOK_SECRET"))
		fmt.Printf("  sig=\"sha256=$(printf '%%s' \"$BODY\" | openssl dgst -sha256 -hmac \"$%s\" -hex | awk '{print $NF}')\"\n", envNameOr(t.TokenEnv, "MESSENGER_HOOK_SECRET"))
		fmt.Printf("  curl -sS -X POST \"http://<host>%s\" -H \"X-Hub-Signature-256: $sig\" -d \"$BODY\"\n", p)
		return nil
	default:
		fmt.Printf("channel %q (kind=%s) needs no connect step\n", name, kind)
		return nil
	}
}

// connectWhatsapp is the wizard for the ONE global device: already linked → report +
// list groups; not linked → run the interactive QR pair.
func connectWhatsapp(name string, t config.Transport) error {
	bin := t.Options["bin"]
	if bin == "" {
		bin = "wacli"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := channel.WhatsappDeviceStatus(ctx, bin)
	if !st.Installed {
		return fmt.Errorf("wacli not found on PATH — install it first (https://wacli.sh)")
	}
	if st.Authenticated {
		fmt.Printf("whatsapp device already linked (%s) — serves every whatsapp channel, no re-pair needed.\n", st.LinkedJID)
		fmt.Println("known groups (bind one with: messenger channel add whatsapp <name> --group <jid>):")
		cmd := exec.Command(bin, "groups", "list")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Println("  (could not list groups — run `wacli groups list` after a sync)")
		}
		return nil
	}
	fmt.Printf("pairing the global whatsapp device via %s auth — scan the QR (once per host):\n", bin)
	cmd := exec.Command(bin, "auth")
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

// channelTest probes connectivity per kind WITHOUT sending a message: whatsapp = global
// device + group known; telegram = getMe with the token (by NAME); webhook = secret
// resolvable. No name = test every configured channel. Never prints a secret value.
func channelTest(args []string) error {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("channel test", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	targets := map[string]config.Transport{}
	if name != "" {
		t, ok := cfg.Transports[name]
		if !ok {
			return fmt.Errorf("no channel named %q", name)
		}
		targets[name] = t
	} else {
		targets = cfg.Transports
	}
	if len(targets) == 0 {
		fmt.Println("no channels configured. add one: messenger channel add <kind> <name>")
		return nil
	}
	res := channel.NewSecretResolver(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	failed := 0
	for _, n := range sortedKeys(targets) {
		t := targets[n]
		spec, err := channel.SpecFor(n, t)
		if err != nil {
			fmt.Printf("✗ %s: %v\n", n, err)
			failed++
			continue
		}
		if spec.Test == nil {
			fmt.Printf("- %s (%s): nothing to test\n", n, spec.Name)
			continue
		}
		lines, err := spec.Test(ctx, n, t, res)
		if err != nil {
			fmt.Printf("✗ %s (%s): %v\n", n, spec.Name, err)
			failed++
			continue
		}
		fmt.Printf("✓ %s (%s)\n", n, spec.Name)
		for _, l := range lines {
			fmt.Printf("    %s\n", l)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d channel(s) failed the connectivity test", failed)
	}
	return nil
}

// cmdInstall installs binary-embedded assets. --skills drops the agent skill into the
// agent skill directories so ANY installed messenger can enable agents to drive it.
func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	skillsFlag := fs.Bool("skills", false, "install the embedded agent skill")
	dir := fs.String("dir", "", "override the skill directory (default ~/.claude/skills)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*skillsFlag {
		return fmt.Errorf("nothing to install — did you mean: messenger install --skills")
	}
	target := *dir
	if target == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		target = filepath.Join(homeDir, ".claude", "skills")
	}
	path, err := skills.Install(target)
	if err != nil {
		return err
	}
	fmt.Printf("installed agent skill: %s\n", path)
	fmt.Println("restart the agent session to pick it up.")
	return nil
}

// probeRunning reports whether a messenger instance already answers on addr — the
// single-instance guard: many installs/agents on one host REUSE the running hub
// instead of double-binding channels (a second telegram webhook consumer or wacli
// stream would split/steal messages).
func probeRunning(addr string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(baseURLFor(addr) + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var h struct {
		Service string `json:"service"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h.Service == "messenger"
}

// baseURLFor turns a listen addr (":14310" or "host:port") into the loopback-default
// base URL CLI verbs use to reach the running hub.
func baseURLFor(addr string) string {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr
}

func envNameOr(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

// --- subscriptions --------------------------------------------------------------------------

func cmdSubscribe(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: messenger subscribe <add|list|remove> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return subscribeList(rest)
	case "add":
		return subscribeAdd(rest)
	case "remove", "rm":
		return subscribeRemove(rest)
	default:
		return fmt.Errorf("unknown subscribe subcommand %q", sub)
	}
}

func subscribeAdd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: messenger subscribe add <name> --url URL [--channels a,b] [--secret-env NAME]")
	}
	name := args[0]
	fs := flag.NewFlagSet("subscribe add", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	url := fs.String("url", "", "consumer URL each envelope is POSTed to")
	channels := fs.String("channels", "", "comma-separated channel names to deliver (empty = all)")
	secretEnv := fs.String("secret-env", "", "env var NAME holding the HMAC signing secret for pushes")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *url == "" {
		return fmt.Errorf("--url is required")
	}
	cfg, path, err := loadOrInitConfig(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Subscriptions == nil {
		cfg.Subscriptions = map[string]config.Subscription{}
	}
	if _, exists := cfg.Subscriptions[name]; exists {
		return fmt.Errorf("subscription %q already exists (remove it first)", name)
	}
	var chans []string
	if *channels != "" {
		for _, c := range strings.Split(*channels, ",") {
			if c = strings.TrimSpace(c); c != "" {
				chans = append(chans, c)
			}
		}
	}
	cfg.Subscriptions[name] = config.Subscription{Enabled: true, URL: *url, Channels: chans, SecretEnv: *secretEnv}
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	fmt.Printf("added subscription %q → %s (channels: %s)\n", name, *url, orAll(chans))
	fmt.Println("  delivery is durable: its cursor advances only on 2xx; a down consumer catches up.")
	if *secretEnv != "" {
		fmt.Printf("  pushes are HMAC-signed with $%s (X-Messenger-Signature-256; value never printed)\n", *secretEnv)
	}
	return nil
}

func subscribeList(args []string) error {
	fs := flag.NewFlagSet("subscribe list", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.Subscriptions) == 0 {
		fmt.Println("no subscriptions. add one: messenger subscribe add <name> --url URL")
		return nil
	}
	names := sortedKeys(cfg.Subscriptions)
	fmt.Printf("%-16s %-8s %-8s %-30s %s\n", "NAME", "ENABLED", "CURSOR", "URL", "CHANNELS")
	for _, n := range names {
		s := cfg.Subscriptions[n]
		fmt.Printf("%-16s %-8v %-8d %-30s %s\n", n, s.Enabled, readCursorFile(home.Path("cursors", n)), s.URL, orAll(s.Channels))
	}
	return nil
}

func subscribeRemove(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: messenger subscribe remove <name>")
	}
	name := args[0]
	fs := flag.NewFlagSet("subscribe remove", flag.ContinueOnError)
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
	if _, ok := cfg.Subscriptions[name]; !ok {
		return fmt.Errorf("no subscription named %q", name)
	}
	delete(cfg.Subscriptions, name)
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("removed subscription %q (cursor file left at %s)\n", name, home.Path("cursors", name))
	return nil
}

func orAll(chans []string) string {
	if len(chans) == 0 {
		return "(all)"
	}
	return strings.Join(chans, ",")
}

// --- listen / send / serve -------------------------------------------------------------------

// cmdListen runs ingress + the subscription dispatcher without the consumer API: inbound
// is appended to the inbox and pushed to every subscription (and optionally an ad-hoc
// --webhook URL). Pushed channels are served on --addr.
func cmdListen(args []string) error {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path (default $MESSENGER_HOME/config.toml)")
	addr := fs.String("addr", ":14310", "address for pushed channel webhooks")
	webhook := fs.String("webhook", "", "ad-hoc subscriber URL to POST each inbound envelope to (no cursor)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if probeRunning(*addr) {
		fmt.Printf("messenger already running on %s — reusing it; not starting a second ingress.\n", *addr)
		return nil
	}
	box, err := inbox.Open(home.InboxPath())
	if err != nil {
		return err
	}
	disp := subscription.New(box, home.Path("cursors"), cfg.Subscriptions)
	rt := channel.NewRuntime(cfg.Enabled(), channel.NewSecretResolver(nil), fanout(box, disp, *webhook))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := rt.Up(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "messenger: listen (partial):", err)
	}
	go disp.Run(ctx)
	srv := &http.Server{Addr: *addr, Handler: rt.HTTPHandler()}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()); rt.Down() }()
	fmt.Printf("messenger listen on %s (channels: %v, subscriptions: %d)\n", *addr, sortedKeys(cfg.Enabled()), disp.Count())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// cmdSend delivers one message and prints the provider-assigned message id (the
// caller's key to thread onto its own send). --file attaches one local path or http(s)
// URL; with it set, --text is optional.
func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	ch := fs.String("channel", "", "channel NAME to send on")
	text := fs.String("text", "", "message text (optional when --file is set)")
	file := fs.String("file", "", "local path or http(s) URL to attach")
	to := fs.String("to", "", "thread/chat/group id to deliver to (default: the channel's configured target)")
	replyTo := fs.String("reply-to", "", "message id this send replies to, or \"last\" for the newest inbound on the channel/thread")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ch == "" || (*text == "" && *file == "") {
		return fmt.Errorf("--channel plus --text or --file are required")
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	// Conversation-first: --reply-to last answers the obvious previous message and
	// inherits its thread, no id bookkeeping.
	if *replyTo == "last" {
		box, berr := inbox.Open(home.InboxPath())
		if berr != nil {
			return berr
		}
		last, ok, lerr := box.Last(*ch, *to)
		if lerr != nil {
			return lerr
		}
		if !ok {
			return fmt.Errorf("no previous message on channel %q to reply to", *ch)
		}
		*replyTo = last.ID
		if *to == "" {
			*to = last.ThreadID
		}
	}
	// The --file shorthand mirrors the /send `file` field: a remote http(s) reference
	// rides as URL, anything else is a local Path; Name is the base, Type "file".
	var attachments []envelope.Attachment
	if *file != "" {
		a := envelope.Attachment{Type: "file", Name: filepath.Base(*file)}
		if strings.HasPrefix(*file, "http://") || strings.HasPrefix(*file, "https://") {
			a.URL = *file
		} else {
			a.Path = *file
		}
		attachments = append(attachments, a)
	}
	env := envelope.Normalize(envelope.Envelope{
		Channel:     *ch,
		Text:        *text,
		ThreadID:    *to,
		ReplyTo:     *replyTo,
		Origin:      "messenger",
		Attachments: attachments,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rt := channel.OpenSend(cfg, channel.NewSecretResolver(nil))
	id, err := rt.Send(ctx, env)
	if err != nil {
		return err
	}
	fmt.Printf("sent id=%s channel=%s\n", id, env.Channel)
	return nil
}

// cmdInject injects one message INTO the running hub through a webhook channel — the
// first-class replacement for the manual openssl+curl HMAC dance. The channel's secret
// is resolved by NAME exactly as the server resolves it (SecretResolver: vault entry,
// else env var), the exact raw body bytes are signed, and the result is POSTed to the
// hub's /webhook/<name>. The secret value never appears in output.
func cmdInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	ch := fs.String("channel", "", "webhook channel NAME to inject on")
	text := fs.String("text", "", "message text")
	sender := fs.String("sender", "", "sender label (default $USER)")
	thread := fs.String("thread", "", "thread id the message belongs on")
	replyTo := fs.String("reply-to", "", "message id this injection replies to")
	addr := fs.String("addr", ":14310", "address of the running hub")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ch == "" || *text == "" {
		return fmt.Errorf("--channel and --text are required")
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	t, ok := cfg.Transports[*ch]
	if !ok {
		return fmt.Errorf("no channel named %q", *ch)
	}
	kind := channel.NormalizeKind(t.Kind, *ch)
	if kind != "webhook" {
		return fmt.Errorf("channel %q is kind %s — inject needs a webhook channel (add one: messenger channel add webhook <name> --token-env NAME)", *ch, kind)
	}
	// Same resolution path as the serving hub; the error names the source, never a value.
	secret, err := channel.NewSecretResolver(nil).Token(t)
	if err != nil {
		return err
	}
	if *sender == "" {
		*sender = os.Getenv("USER")
	}
	body, id, err := injectBody(*text, *sender, *thread, *replyTo)
	if err != nil {
		return err
	}
	path := t.Options["path"]
	if path == "" {
		path = "/webhook/" + *ch
	}
	sigHeader := t.Options["signatureHeader"]
	if sigHeader == "" {
		sigHeader = "X-Hub-Signature-256"
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if err := injectPost(client, baseURLFor(*addr), path, sigHeader, []byte(secret), body); err != nil {
		return err
	}
	fmt.Printf("injected id=%s channel=%s\n", id, *ch)
	return nil
}

// injectBody builds the exact raw bytes that are signed and posted — the webhook body
// shape the hub normalizes (references/webhook.md), empties omitted. The id is minted
// here so the caller can print the same identity the inbox envelope carries.
func injectBody(text, sender, thread, replyTo string) ([]byte, string, error) {
	id := envelope.Normalize(envelope.Envelope{}).ID
	p := struct {
		ID       string `json:"id"`
		Text     string `json:"text"`
		Sender   string `json:"sender,omitempty"`
		ThreadID string `json:"thread_id,omitempty"`
		ReplyTo  string `json:"reply_to,omitempty"`
	}{ID: id, Text: text, Sender: sender, ThreadID: thread, ReplyTo: replyTo}
	body, err := json.Marshal(p)
	return body, id, err
}

// injectPost signs body under secret and POSTs it to the hub at baseURL+path. Split
// from cmdInject so tests can stand an httptest server in for the hub. Failures come
// back with a one-line hint; the secret value never enters an error.
func injectPost(client *http.Client, baseURL, path, sigHeader string, secret, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(sigHeader, channel.SignHMAC(secret, body))
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("hub unreachable at %s (start it: messenger serve): %w", baseURL, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("hub rejected the signature (401) — the hub resolves a different secret for this channel than this shell")
	default:
		return fmt.Errorf("hub answered %d for %s", resp.StatusCode, path)
	}
}

// cmdServe runs everything on one port: channel webhooks, the consumer API, and the
// subscription dispatcher.
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
	// Single instance per host: if a messenger already answers here, reuse it — a second
	// instance would double-bind channels (split telegram webhooks, a second wacli stream).
	if probeRunning(*addr) {
		fmt.Printf("messenger already running on %s — reusing it (POST /send, GET /inbox there).\n", *addr)
		fmt.Println("to run a second isolated instance, pass a different --addr and $MESSENGER_HOME.")
		return nil
	}
	box, err := inbox.Open(home.InboxPath())
	if err != nil {
		return err
	}
	disp := subscription.New(box, home.Path("cursors"), cfg.Subscriptions)
	rt := channel.NewRuntime(cfg.Enabled(), channel.NewSecretResolver(nil), fanout(box, disp, ""))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := rt.Up(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "messenger: serve (partial):", err)
	}
	go disp.Run(ctx)

	// Resolve the bearer token by NAME once, host-only; the value never enters a log.
	token := ""
	if cfg.ServeTokenEnv != "" {
		token = os.Getenv(cfg.ServeTokenEnv)
	}
	srv := server.New(rt, box, token)
	hs := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() { <-ctx.Done(); _ = hs.Shutdown(context.Background()); rt.Down() }()
	fmt.Printf("messenger serve on %s (channels: %v, subscriptions: %d, auth: %v)\n",
		*addr, sortedKeys(cfg.Enabled()), disp.Count(), token != "")
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// fanout builds the Publisher: append to the inbox, wake the subscription dispatcher,
// and (if set) POST the envelope to an ad-hoc webhook. A push failure is logged, never fatal.
func fanout(box *inbox.Inbox, disp *subscription.Dispatcher, webhook string) channel.Publisher {
	client := &http.Client{Timeout: 10 * time.Second}
	return func(env envelope.Envelope) {
		if err := box.Append(env); err != nil {
			fmt.Fprintln(os.Stderr, "messenger: inbox append:", err)
		}
		if disp != nil {
			disp.Notify()
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

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
