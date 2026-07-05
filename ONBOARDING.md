# messenger — onboarding (zero → working hub)

Follow this top to bottom on a fresh machine and you end with a running conversation
hub, at least one working channel, a consumer receiving messages, and the agent skill
installed. Deeper docs: `README.md` (index), `docs/SPEC.md`, `docs/ARCHITECTURE.md`,
`docs/API.md`, `skills/messenger/` (agent recipes + per-kind references).

## 1. Prerequisites

- **Go 1.22+** (build only — the artifact is one static binary, `CGO_ENABLED=0`).
- **wacli** on PATH *only if* you want WhatsApp (https://wacli.sh). Telegram/webhook
  need nothing extra.
- A place on PATH for the binary (`~/.local/bin` assumed below).

## 2. Build + install

```sh
git clone git@github.com:deemwar-products/messenger.git && cd messenger
CGO_ENABLED=0 go build -o ~/.local/bin/messenger ./cmd/messenger
messenger install --skills        # embedded agent skill → ~/.claude/skills/messenger
```

Sanity: `messenger help` prints the verb list.

## 3. Scaffold + secrets

```sh
messenger setup                   # creates ~/.config/messenger/config.toml (+ prints device state)
```

Secrets are referenced by **NAME** and read from the process environment at the point
of use — a value never goes in config, code, or output. Export what you'll use (values
are your own; these are the conventional names):

```sh
export TELEGRAM_BOT_TOKEN=YOUR_BOT_TOKEN_HERE       # per telegram bot
export MESSENGER_HOOK_SECRET=YOUR_SHARED_SECRET     # per webhook channel
export MESSENGER_SERVE_TOKEN=YOUR_API_BEARER        # protects /send + /inbox (optional on loopback)
```

Put them wherever the hub process starts (shell profile, launchd/systemd environment).

## 4. Add channels (pick what you need)

**WhatsApp** — ONE paired device per host; each channel = a group:

```sh
messenger channel connect anything-yet-to-exist 2>/dev/null || true   # optional peek
messenger channel add whatsapp ops --group 123456789@g.us
messenger channel connect ops     # already linked → prints JID + group list; else ONE QR pair
```

Don't know the group JID? `messenger channel connect ops` lists known groups (run
`wacli sync` once if the list is empty). A whatsapp channel with **no** `--group` is
the catch-all for DMs/unlisted groups.

**Telegram** — one bot per channel (create the bot via @BotFather first):

```sh
messenger channel add telegram mybot --token-env TELEGRAM_BOT_TOKEN --chat-id -1001234567890
messenger channel connect mybot --public-url https://your-host   # prints the setWebhook curl — run it
```

Inbound needs the hub reachable at that public URL (tunnel/reverse proxy); outbound
works immediately.

**Webhook** — for scripts/CI/bridges:

```sh
messenger channel add webhook incoming --token-env MESSENGER_HOOK_SECRET
```

## 5. Verify BEFORE running

```sh
messenger channel test            # whatsapp device+group · telegram getMe · webhook secret
messenger status                  # config, channels, device, inbox, subscriptions
```

Fix anything ✗ now — `test` exits non-zero and names the problem (e.g. "env
TELEGRAM_BOT_TOKEN is unset", "group … not in the local store").

## 6. Run the hub (ONE per host)

```sh
messenger serve                   # :14310 — channel webhooks + /send + /inbox + /media + dispatcher
```

It probes first and **reuses** a running hub instead of double-starting. For boot
persistence wrap exactly this command in launchd/systemd with the env vars from step 3.

Smoke test:

```sh
curl -sS http://127.0.0.1:14310/health     # {"ok":true,"service":"messenger","channels":{...}}
messenger send --channel ops --text "hub is up"
messenger send --channel ops --file ./README.md --text "with an attachment"
```

## 7. Wire a consumer

```sh
messenger subscribe add myapp --url http://localhost:9000/hook            # all channels
```

Your endpoint gets every envelope POSTed in order; answer 2xx. Down = automatic
catch-up from your cursor (at-least-once — dedupe by envelope `id`). Reply to anything
with `POST /send {"channel":..., "text":..., "reply_to":"last"}`. Ad-hoc scripts can
poll `GET /inbox?since=N` instead. Attachments ride the envelope; fetch bytes via
`GET /media/<basename of attachments[].path>`.

## 8. For AI agents

`messenger install --skills` (already done in step 2) gives any Claude Code session the
full playbook: hub detection/reuse, send/reply/poll/subscribe recipes, per-kind
references. Restart the agent session once to pick it up.

## Troubleshooting

| symptom | fix |
|---|---|
| `messenger serve` says "already running — reusing" | That's correct behavior. Talk to the running hub; don't start a second. |
| whatsapp channel silent | `messenger channel test <name>`: device linked? group JID known? Then check `wacli doctor --json` (`connected` state) and the serve log ("whatsapp stream exited … restarting"). |
| telegram inbound silent, outbound fine | setWebhook never ran or the public URL isn't reachable — re-run `channel connect <name> --public-url …` and execute the printed curl. |
| `401` on /send, /inbox, /media | Send `Authorization: Bearer $MESSENGER_SERVE_TOKEN` (the env var named by `serveTokenEnv`). |
| `401 signature verification failed` on /webhook/<name> | Sign the EXACT raw bytes you POST (no re-encoding); header `X-Hub-Signature-256: sha256=<hex>`. |
| `409 no previous message … to reply to` | The conversation is empty — send without `reply_to`. |
| token errors on send | The env var named by `--token-env` isn't set in the HUB's environment (not just your shell). |
| second machine, same WhatsApp number | Don't. One paired device per host; point other machines at this hub over HTTP. |

## Green gate (before any commit)

```sh
CGO_ENABLED=0 go build ./... && go vet ./... && CGO_ENABLED=0 go test ./...
```
