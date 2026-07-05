# messenger — host setup (so you can use it later)

Do this ONCE per host. At the end you have a single messenger hub running as a managed
OS service (starts on boot, restarts on crash) that sends **and receives** over your
channels, and any agent (CEO, crypto desk, a script) can drive it over HTTP. How agents
*use* it: `skills/messenger/references/agents.md`. Deeper docs: `README.md`,
`docs/SPEC.md` · `docs/ARCHITECTURE.md` · `docs/API.md`.

> **The one rule that makes all of this work:** ONE hub per host. Nothing else runs
> `messenger serve` or `wacli` — the WhatsApp device is a host-global singleton, and a
> second `wacli sync` steals it and silently drops inbound. The hub is the sole owner.

## 1. Install the binary

```sh
# from a release (no Go needed): download messenger-<os>-<arch> from
#   https://github.com/deemwar-products/messenger/releases  → chmod +x → put on PATH
# or with Go:
go install github.com/deemwar-products/messenger/cmd/messenger@latest   # → ~/go/bin
# or build from source:
git clone git@github.com:deemwar-products/messenger.git && cd messenger
CGO_ENABLED=0 go build -o ~/.local/bin/messenger ./cmd/messenger

messenger help          # sanity: prints the verbs
messenger install --skills   # drop the agent skill into ~/.claude/skills (for AI agents)
```

State lives in `~/.config/messenger/` on every platform (`config.toml`, `inbox.ndjson`,
`cursors/`, `media/`). Override with `$MESSENGER_HOME`.

## 2. Scaffold + secrets

```sh
messenger setup    # creates ~/.config/messenger/config.toml + prints the WhatsApp device state
```

Secrets are referenced by **NAME**, never a value — resolved from the environment where
the **hub** runs. Put them in `~/.config/messenger/serve-token.env` (the service sources
it at launch; gitignored, never copied into the unit):

```sh
# ~/.config/messenger/serve-token.env  — real values here, never printed/committed
export MESSENGER_SERVE_TOKEN=YOUR_API_BEARER      # protects /send + /inbox + /media
export MESSENGER_HOOK_SECRET=YOUR_SHARED_SECRET   # per webhook channel
export TELEGRAM_BOT_TOKEN=YOUR_BOT_TOKEN          # per telegram bot (if used)
```

## 3. WhatsApp: install the prerequisite + pair (skip if you only use telegram/webhook)

wacli (the WhatsApp engine) is a **manual prerequisite** — messenger never auto-installs
it, but it can install it for you explicitly:

```sh
messenger install --wacli          # brew install openclaw/tap/wacli (macOS); prints the line elsewhere
messenger channel add whatsapp <name> --group <group-jid>   # bind a channel to a group
messenger channel connect <name>   # already linked → shows JID + FREE groups; else scan the QR ONCE
```

- The device is **global** — pair once, it serves every whatsapp channel.
- **One group = one channel.** A chat with no bound channel is **dropped** (no
  catch-all). `channel connect` lists only the FREE (unbound) groups so you can pick a JID.
- To remove WhatsApp later: `messenger uninstall --wacli` (unlinks the device, then removes wacli).

## 4. Telegram / webhook channels (as needed)

```sh
# telegram — one bot per channel (create it via @BotFather first):
messenger channel add telegram mybot --token-env TELEGRAM_BOT_TOKEN --chat-id -1001234567890
messenger channel connect mybot --public-url https://your-host    # prints the setWebhook curl — run it

# webhook — a signed inbound path for scripts/CI/bridges:
messenger channel add webhook incoming --token-env MESSENGER_HOOK_SECRET
```

Telegram inbound needs the hub reachable at the public URL (tunnel/reverse proxy);
outbound works immediately.

## 5. Verify before running

```sh
messenger channel test    # whatsapp device+group · telegram getMe · webhook secret — exits non-zero on failure
messenger channel list
```

## 6. Run the hub as a service (the "use it later" part)

```sh
source ~/.config/messenger/serve-token.env    # so install picks up the secrets' presence
messenger install --service                    # launchd (macOS) / systemd --user (Linux)
```

This installs ONE managed hub that **starts on boot and restarts on crash**, sourcing
your `serve-token.env` at launch (only its path is in the unit, never a value) and baking
your PATH so it can find `wacli`. It refuses if a hub is already running.

```sh
curl -sS http://127.0.0.1:14310/health    # {"ok":true,"service":"messenger","channels":{…}}
messenger status                          # shows: server RUNNING, channels, device, inbox, subscriptions
messenger uninstall --service             # remove it cleanly when you want
```

(For a quick dev run without a service: `messenger serve` — it probes and reuses a
running hub instead of double-starting.)

## 7. Onboard your consumers (CEO, crypto desk, …)

One idempotent command per agent gives it a lane + a durable listen, and prints its exact
send/reply/receive contract:

```sh
# a whatsapp group lane:
messenger register cryptodesk --group 120363410186820001@g.us --url http://127.0.0.1:9100/hook
# a telegram bot lane:
messenger register ceo --kind telegram --token-env CEO_BOT_TOKEN --url http://127.0.0.1:9000/hook
# a webhook lane (scripted):
messenger register ci --kind webhook --token-env CI_HOOK_SECRET --url http://127.0.0.1:9300/hook
```

Each consumer then receives every envelope for its channels POSTed to its URL, in order,
and replies via `POST /send`. Full agent-side contract: `references/agents.md`. Restart
the hub after adding channels/subscriptions so it reloads: `messenger uninstall --service
&& messenger install --service` (or just `install --service` again).

## Troubleshooting

| symptom | fix |
|---|---|
| WhatsApp went silent | `pgrep -f "wacli.*sync"` → more than one = two hubs fighting the device. Kill all but the service; `messenger status`. |
| `install --service` refuses | A hub is already running — stop it first (`pkill -f "messenger serve"` or `uninstall --service`), then reinstall. |
| service up but `wacli: not found` | Reinstall the service from a shell where `which wacli` works — it bakes that PATH. |
| whatsapp channel receives nothing | `messenger channel test <name>` — device linked? group JID bound? A no-group channel receives nothing by design. |
| telegram inbound silent, outbound fine | setWebhook never ran / public URL unreachable — re-run `channel connect <name> --public-url …` and run the printed curl. |
| `401` on /send, /inbox, /media | Send `Authorization: Bearer $MESSENGER_SERVE_TOKEN`. |
| channel added but hub doesn't see it | The hub loads config at start — restart it (`install --service` again). |
| second machine, same WhatsApp number | Don't. One device per host; point other machines at this hub over HTTP. |
