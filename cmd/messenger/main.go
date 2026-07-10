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
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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
	"github.com/deemwar-products/messenger/service"
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
	case "uninstall":
		err = cmdUninstall(os.Args[2:])
	case "register":
		err = cmdRegister(os.Args[2:])
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
  messenger install --skills | --wacli | --service    install: agent skill / wacli prereq / hub as an OS service
  messenger uninstall --wacli | --service             uninstall the above
  messenger register <agent> [--group <jid> | --kind telegram|webhook --token-env NAME [--chat-id ID]]
                             [--channels a,b] [--url URL] [--secret-env NAME]
                                           one-shot agent onboarding: lane (any kind) + listen (idempotent)
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
            strict routing — a chat with no bound channel is dropped (no catch-all)
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
	printKindStatus()
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

// printKindStatus reports every kind's host-level state (whatsapp: the global device's
// pair state) — setup/status ask each kind polymorphically.
func printKindStatus() {
	for _, n := range channel.KindNames() {
		for _, line := range channel.Kinds()[n].Status() {
			fmt.Println(line)
		}
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
	printKindStatus()

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
		return fmt.Errorf("usage: messenger channel <add|list|remove|connect|test|teams-create> ...")
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
	case "teams-create":
		return channelTeamsCreate(rest)
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
		detail := ""
		if k, err := channel.KindFor(n, t); err == nil {
			detail = k.Detail(n, t)
		}
		fmt.Printf("%-16s %-10s %-8v %s\n", n, channel.NormalizeKind(t.Kind, n), t.Enabled, detail)
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
	userEnv := fs.String("user-env", "", "env var NAME holding the user/app identity (teams: the App ID)")
	conversation := fs.String("conversation", "", "teams: default target conversation id")
	group := fs.String("group", "", "whatsapp: the group JID this channel is bound to")
	disabled := fs.Bool("disabled", false, "add the channel disabled")
	opts := optionFlags{}
	fs.Var(&opts, "option", "repeatable free-form option k=v")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	k, ok := channel.Kinds()[kind]
	if !ok {
		return fmt.Errorf("unknown kind %q (%s)", args[0], strings.Join(channel.KindNames(), "|"))
	}
	if k.Traits().RequiresToken && *tokenEnv == "" && *tokenVault == "" {
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
	if *conversation != "" {
		opts["conversationId"] = *conversation
	}
	if *group != "" {
		opts["group"] = *group
	}
	if len(opts) == 0 {
		opts = nil
	}
	want := config.Transport{
		Enabled:    !*disabled,
		Kind:       kind,
		Account:    *account,
		TokenEnv:   *tokenEnv,
		TokenVault: *tokenVault,
		UserEnv:    *userEnv,
		Options:    opts,
	}
	if err := k.Validate(name, want, cfg.Transports); err != nil {
		return err
	}
	cfg.Transports[name] = want
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	fmt.Printf("added channel %q (kind=%s, enabled=%v) to %s\n", name, kind, !*disabled, path)
	if *tokenEnv != "" {
		fmt.Printf("  remember to export %s (value never printed)\n", *tokenEnv)
	}
	for _, h := range k.AddHints(name, want) {
		fmt.Println("  " + h)
	}
	return nil
}

// channelTeamsCreate creates a Teams channel via Graph (RSC Channel.Create.Group), borrowing
// the bot credentials from an existing teams channel (one bot per host), and — because the
// bot CREATED the channel — knows its conversationId immediately. With --as it registers a
// bound messenger teams channel in one shot, no @mention / discovery needed.
func channelTeamsCreate(args []string) error {
	fs := flag.NewFlagSet("channel teams-create", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	team := fs.String("team", "", "Teams team (group) id to create the channel in")
	name := fs.String("name", "", "display name for the new Teams channel")
	desc := fs.String("desc", "", "optional channel description")
	as := fs.String("as", "", "also register a messenger teams channel of this name bound to the new conversation")
	from := fs.String("from", "", "teams channel name to borrow bot credentials from (default: first teams channel)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *team == "" || *name == "" {
		return fmt.Errorf("usage: messenger channel teams-create --team <teamId> --name <display> [--as <channel>] [--desc ...]")
	}
	cfg, path, err := loadOrInitConfig(*cfgPath)
	if err != nil {
		return err
	}
	// Borrow bot credentials from an existing teams channel (one bot per host).
	var creds config.Transport
	credName := ""
	for _, n := range sortedKeys(cfg.Transports) {
		t := cfg.Transports[n]
		if t.Kind != "teams" {
			continue
		}
		if *from == "" || *from == n {
			creds, credName = t, n
			if *from == n {
				break
			}
		}
	}
	if credName == "" {
		return fmt.Errorf("no teams channel to borrow bot credentials from — add one first: messenger channel add teams teams --token-env NAME --user-env NAME --option tenantId=...")
	}
	res := channel.NewSecretResolver(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	convID, err := channel.CreateTeamsChannel(ctx, creds, res, *team, *name, *desc)
	if err != nil {
		return err
	}
	fmt.Printf("created Teams channel %q in team %s\n", *name, *team)
	fmt.Printf("  conversationId: %s\n", convID)

	tenantFlag := ""
	if tid := creds.Options["tenantId"]; tid != "" {
		tenantFlag = " --option tenantId=" + tid
	}
	if *as == "" {
		fmt.Printf("  bind it: messenger channel add teams <name> --token-env %s --user-env %s%s --conversation %s\n",
			creds.TokenEnv, creds.UserEnv, tenantFlag, convID)
		return nil
	}
	if _, exists := cfg.Transports[*as]; exists {
		return fmt.Errorf("channel %q already exists (remove it first); the new conversationId is %s", *as, convID)
	}
	opts := map[string]string{"conversationId": convID}
	if tid := creds.Options["tenantId"]; tid != "" {
		opts["tenantId"] = tid
	}
	want := config.Transport{
		Enabled:    true,
		Kind:       "teams",
		TokenEnv:   creds.TokenEnv,
		TokenVault: creds.TokenVault,
		UserEnv:    creds.UserEnv,
		UserVault:  creds.UserVault,
		Options:    opts,
	}
	if err := channel.Kinds()["teams"].Validate(*as, want, cfg.Transports); err != nil {
		return err
	}
	cfg.Transports[*as] = want
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	fmt.Printf("  bound channel %q -> conversation %s (saved to %s)\n", *as, convID, path)
	fmt.Printf("  send into it: messenger send --channel %s --text \"hi\"\n", *as)
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
	k, err := channel.KindFor(name, t)
	if err != nil {
		return err
	}
	return k.Connect(name, t, channel.ConnectParams{PublicURL: *publicURL, Existing: cfg.Transports})
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
		k, err := channel.KindFor(n, t)
		if err != nil {
			fmt.Printf("✗ %s: %v\n", n, err)
			failed++
			continue
		}
		lines, err := k.Test(ctx, n, t, res)
		if err != nil {
			fmt.Printf("✗ %s (%s): %v\n", n, k.Name(), err)
			failed++
			continue
		}
		fmt.Printf("✓ %s (%s)\n", n, k.Name())
		for _, l := range lines {
			fmt.Printf("    %s\n", l)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d channel(s) failed the connectivity test", failed)
	}
	return nil
}

// cmdInstall installs one of: the embedded agent skill (--skills), the wacli WhatsApp
// prerequisite (--wacli), or the hub as an OS service (--service).
func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	skillsFlag := fs.Bool("skills", false, "install the embedded agent skill")
	wacliFlag := fs.Bool("wacli", false, "install the wacli WhatsApp prerequisite (brew on macOS)")
	serviceFlag := fs.Bool("service", false, "install the hub as an OS service (launchd/systemd), start on boot")
	dir := fs.String("dir", "", "override the skill directory (default ~/.claude/skills)")
	addr := fs.String("addr", ":14310", "service: address the hub serves on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *skillsFlag:
		return installSkills(*dir)
	case *wacliFlag:
		return installWacli()
	case *serviceFlag:
		return installService(*addr)
	default:
		return fmt.Errorf("nothing to install — one of: --skills | --wacli | --service")
	}
}

func installSkills(dir string) error {
	target := dir
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

// installWacli ensures the wacli WhatsApp prerequisite is present. It runs the real
// package command (brew on macOS) — never a buried curl|bash — and otherwise prints the
// exact instruction. It never handles a secret. Then it reports the device pair state.
func installWacli() error {
	if p, err := exec.LookPath("wacli"); err == nil {
		fmt.Printf("wacli already installed: %s\n", p)
	} else {
		switch runtime.GOOS {
		case "darwin":
			if _, err := exec.LookPath("brew"); err != nil {
				return fmt.Errorf("Homebrew not found — install wacli manually: see https://wacli.sh")
			}
			fmt.Println("installing wacli via Homebrew (openclaw/tap/wacli)…")
			cmd := exec.Command("brew", "install", "openclaw/tap/wacli")
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("brew install failed: %w", err)
			}
		default:
			return fmt.Errorf("auto-install is macOS-only — install wacli manually:\n  see https://wacli.sh (repo github.com/openclaw/wacli)")
		}
	}
	// Report device state + next step; pairing stays the explicit `channel connect` flow.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := channel.WhatsappDeviceStatus(ctx, "")
	switch {
	case st.Authenticated:
		fmt.Printf("whatsapp device already linked (%s) — add group channels: messenger channel add whatsapp <name> --group <jid>\n", st.LinkedJID)
	case st.Installed:
		fmt.Println("wacli ready but not paired — pair once: messenger channel connect <a-whatsapp-channel>")
	default:
		fmt.Println("wacli still not on PATH — open a new shell, then: messenger channel connect <name>")
	}
	return nil
}

// installService installs the hub as an OS service (launchd/systemd), sourcing the
// existing secret env file so no value lands in the unit. It refuses if a manually
// started hub is already up (stop it first, so the service owns the single instance).
func installService(addr string) error {
	if probeRunning(addr) {
		return fmt.Errorf("a hub is already running on %s — stop it first (so the service owns the one instance), then re-run", addr)
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	envFile := home.Path("serve-token.env") // sourced at launch if present; only its PATH is in the unit
	if _, statErr := os.Stat(envFile); statErr != nil {
		envFile = ""
	}
	if err := service.Install(service.Config{Bin: bin, Addr: addr, Home: home.Dir(), EnvFile: envFile, Path: os.Getenv("PATH")}); err != nil {
		return err
	}
	fmt.Printf("installed messenger as a service (%s) — starts on boot, restarts on crash.\n", runtime.GOOS)
	if envFile != "" {
		fmt.Printf("  secrets sourced from %s at launch (never copied into the unit)\n", envFile)
	}
	fmt.Println("  manage it: messenger install --service (reinstall) · messenger uninstall --service")
	return nil
}

// cmdUninstall reverses install: --wacli removes the prerequisite (and unlinks the
// device), --service removes the OS service.
func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	wacliFlag := fs.Bool("wacli", false, "uninstall wacli + unlink the WhatsApp device")
	serviceFlag := fs.Bool("service", false, "remove the hub OS service")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *serviceFlag:
		if err := service.Uninstall(); err != nil {
			return err
		}
		fmt.Println("removed the messenger service.")
		return nil
	case *wacliFlag:
		return uninstallWacli()
	default:
		return fmt.Errorf("nothing to uninstall — one of: --wacli | --service")
	}
}

func uninstallWacli() error {
	bin := "wacli"
	if _, err := exec.LookPath(bin); err != nil {
		fmt.Println("wacli is not installed — nothing to do.")
		return nil
	}
	// Unlink the device first so no dangling linked device is left on the phone.
	fmt.Println("unlinking the WhatsApp device (wacli logout)…")
	lo := exec.Command(bin, "logout")
	lo.Stdout, lo.Stderr = os.Stdout, os.Stderr
	_ = lo.Run() // best-effort; proceed to removal regardless
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("brew"); err == nil {
			fmt.Println("removing wacli via Homebrew…")
			cmd := exec.Command("brew", "uninstall", "wacli")
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("brew uninstall failed: %w", err)
			}
			return nil
		}
	}
	fmt.Println("device unlinked. Remove the wacli binary with your package manager (e.g. brew uninstall wacli).")
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

// cmdRegister is one-shot agent onboarding: give an agent a lane (its own channel,
// created when --group names a whatsapp group) and a listen (its own subscription when
// --url is given; poll instructions otherwise), then print exactly how the agent sends,
// replies, and receives. Idempotent: re-running updates the subscription and accepts an
// existing identical channel, so boot scripts can run it every boot.
func cmdRegister(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: messenger register <agent> [--group <jid> | --kind telegram|webhook --token-env NAME [--chat-id ID]] [--channels a,b] [--url URL] [--secret-env NAME]")
	}
	name := args[0]
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	kindFlag := fs.String("kind", "", "lane kind to create ("+strings.Join(channel.KindNames(), "|")+"); --group implies whatsapp")
	group := fs.String("group", "", "whatsapp group JID — creates channel <agent> bound to it")
	tokenEnv := fs.String("token-env", "", "env var NAME holding the lane's token/secret (telegram bot, webhook HMAC)")
	chatID := fs.String("chat-id", "", "telegram: the lane's default target chat id")
	channels := fs.String("channels", "", "existing channel names the agent listens to (default: its own)")
	url := fs.String("url", "", "agent's push endpoint (omit = agent polls /inbox)")
	secretEnv := fs.String("secret-env", "", "env var NAME that HMAC-signs pushes to the agent")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, path, err := loadOrInitConfig(*cfgPath)
	if err != nil {
		return err
	}

	// Lane: the kind builds the agent's own channel polymorphically — whatsapp binds a
	// group, telegram is its own bot, webhook its own signed path.
	kind := channel.NormalizeKind(*kindFlag, *kindFlag)
	if kind == "" && *group != "" {
		kind = "whatsapp"
	}
	if kind != "" {
		k, ok := channel.Kinds()[kind]
		if !ok {
			return fmt.Errorf("unknown kind %q (%s)", *kindFlag, strings.Join(channel.KindNames(), "|"))
		}
		want, hints, err := k.Lane(name, channel.LaneParams{Group: *group, TokenEnv: *tokenEnv, ChatID: *chatID}, cfg.Transports)
		if err != nil {
			return err
		}
		if have, exists := cfg.Transports[name]; exists {
			if !channel.LaneMatches(name, have, want) {
				return fmt.Errorf("channel %q already exists with a different kind/target — remove it first", name)
			}
		} else {
			cfg.Transports[name] = want
			for _, h := range hints {
				fmt.Println(h)
			}
		}
	}

	// Listen: the agent's own subscription (updated in place — registration is idempotent).
	var chans []string
	if *channels != "" {
		for _, c := range strings.Split(*channels, ",") {
			if c = strings.TrimSpace(c); c != "" {
				chans = append(chans, c)
			}
		}
	} else if _, ok := cfg.Transports[name]; ok {
		chans = []string{name} // default lane: its own channel
	}
	for _, c := range chans {
		if _, ok := cfg.Transports[c]; !ok {
			return fmt.Errorf("channel %q does not exist (add it first, or pass --group to create one)", c)
		}
	}
	if *url != "" {
		if cfg.Subscriptions == nil {
			cfg.Subscriptions = map[string]config.Subscription{}
		}
		cfg.Subscriptions[name] = config.Subscription{Enabled: true, URL: *url, Channels: chans, SecretEnv: *secretEnv}
		fmt.Printf("subscription %q → %s (channels: %s)\n", name, *url, orAll(chans))
	}
	if err := saveConfig(path, cfg); err != nil {
		return err
	}

	// The agent's exact contract — print it so onboarding is copy-paste.
	fmt.Printf(`
agent %q is registered. Its contract (also in AGENTS.md):
  discover (never serve):  curl -sf http://127.0.0.1:14310/health
  send:   curl -sS -X POST http://127.0.0.1:14310/send -H "Authorization: Bearer $MESSENGER_SERVE_TOKEN" \
            -d '{"channel":"%s","text":"hello"}'
  reply:  same, plus "reply_to":"<envelope id>" (or "last")
`, name, firstOr(chans, "<channel>"))
	if *url != "" {
		fmt.Printf("  listen: envelopes for %s are POSTed to %s in order — answer 2xx, dedupe by id;\n          restart the hub (or wait for its next boot) if it is already running so the subscription loads.\n", orAll(chans), *url)
	} else {
		fmt.Printf("  listen: poll GET /inbox?since=N (persist the returned next); no push endpoint registered.\n")
	}
	return nil
}

func firstOr(list []string, fallback string) string {
	if len(list) > 0 {
		return list[0]
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
	rt.SetSelfURL(selfURLFromAddr(*addr))

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
	rt.SetSelfURL(selfURLFromAddr(*addr))

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
	srv.UseHookSecret(cfg.HookSecretEnv)
	hs := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() { <-ctx.Done(); _ = hs.Shutdown(context.Background()); rt.Down() }()
	fmt.Printf("messenger serve on %s (channels: %v, subscriptions: %d, auth: %v)\n",
		*addr, sortedKeys(cfg.Enabled()), disp.Count(), token != "")
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// selfURLFromAddr turns a listen addr (":14310", "0.0.0.0:14310", "1.2.3.4:80") into the
// hub's loopback base URL, so a WebhookInbound stream (wacli) POSTs inbound to the local
// hub regardless of the bind host.
func selfURLFromAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "14310"
	}
	return "http://127.0.0.1:" + port
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
