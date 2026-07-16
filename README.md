# messenger

[![Go Reference](https://pkg.go.dev/badge/github.com/deemwar-products/messenger.svg)](https://pkg.go.dev/github.com/deemwar-products/messenger)

Broker-free **conversation hub** as one static Go binary. It owns receiving and sending
messages over **telegram**, **whatsapp**, and generic **webhook** channels, and delivers
inbound messages to N consumers with durable per-consumer cursors — so any product (a
trading desk, an agent OS, a script) plugs in over plain HTTP. No broker. The CLI is the
whole interface.

**Docs:** [`ONBOARDING.md`](ONBOARDING.md) (humans: zero→working) ·
[`AGENTS.md`](AGENTS.md) (the contract for agents & boot scripts — one hub, register, never serve) ·
[`docs/SPEC.md`](docs/SPEC.md) (product model) ·
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) (packages, flows, design decisions) ·
[`docs/API.md`](docs/API.md) (HTTP surface, exact shapes) ·
[`skills/messenger/`](skills/messenger/) (agent skill + per-kind references)

```
ports (channels)           hub                       consumers (subscriptions)
telegram bot A ─┐                                     ┌─> factory    POST url (own cursor)
telegram bot B ─┤→ Envelope → inbox.ndjson ─────────→ ├─> cryptodesk POST url (own cursor)
whatsapp device ┤   {id, channel, sender, text,       └─> ad-hoc: GET /inbox?since=N
webhook hooks  ─┘    thread_id, reply_to, ts}
                 ← POST /send {channel,text,to,reply_to} → returns the provider message id
```

## Quick start

```sh
go build -o messenger ./cmd/messenger        # CGO_ENABLED=0, single static binary
# on macOS prefer:  ./scripts/build.sh        # + ad-hoc codesign (avoids "Killed: 9" after a rebuild)

messenger setup                              # scaffold ~/.config/messenger + guide
messenger channel add whatsapp ops --group 123456789@g.us
messenger channel add telegram mybot --token-env TELEGRAM_BOT_TOKEN --chat-id -1001234567890
messenger channel add webhook incoming --token-env MESSENGER_HOOK_SECRET
messenger channel connect ops                # whatsapp: QR pair ONCE per host (or "already linked")
messenger subscribe add factory --url http://localhost:9000/hook --channels mybot,ops
messenger channel test                       # probe every channel (device, getMe, secrets)
messenger serve                              # everything on :14310 (reuses a running hub)
messenger send --channel ops --file report.pdf --text "caption"   # attachments in one flag
messenger status                             # one-glance health
messenger install --skills                   # drop the embedded agent skill into ~/.claude/skills
```

**One hub per host:** `serve` probes the address first and reuses an already-running
messenger instead of double-starting — a second instance would split telegram webhooks
and spawn a competing whatsapp stream. Every install/agent talks to the one hub over HTTP.

**Extensible:** a new kind (teams, slack, …) is one file implementing `channel.Channel`
that embeds `channel.Base` + `channel.Register(...)` — CLI, runtime, `/send`, subscriptions, and
threading work unchanged. See `docs/SPEC.md`.

## Channel kinds

| kind | multiplicity | notes |
|------|--------------|-------|
| `telegram` | many channels, each its OWN bot | own `--token-env`, default `--chat-id` |
| `whatsapp` | ONE global paired device; each channel = a GROUP | `--group <jid>` binds the channel to a group; ONE wacli stream serves them all; a channel with no group is the catch-all; `connect` detects an already-linked device and lists groups |
| `webhook` | many channels, each its own hook | HMAC-signed inbound on `/webhook/<name>` (legacy `/hook/<name>` still answers), signed outbound to `callbackURL` |

## Subscriptions (durable consumer delivery)

`messenger subscribe add <name> --url URL [--channels a,b] [--secret-env NAME]` — every
inbound envelope is POSTed in order to the URL; the per-consumer cursor
(`$MESSENGER_HOME/cursors/<name>`) advances only on 2xx, so a consumer that was down
catches up. At-least-once, retried with backoff. Pushes are HMAC-signed
(`X-Messenger-Signature-256`) when `--secret-env` names a secret.

## HTTP API (`messenger serve`)

- `POST /send` — `{channel, text?, file?|attachments?, to?, reply_to?}` → `{ok, id}`
  where `id` is the provider message id (bearer-auth when `serveTokenEnv` set)
- `GET /inbox?since=N` — `{messages, next}`; `N` is a 1-based offset, pass `next` back
- `GET /media/<file>` — inbound media bytes (what an envelope's `attachments[].path`
  names), bearer-auth'd like `/inbox`
- `GET /health` — `{ok, channels: {name: kind}}`

## Threaded replies

Each inbound Envelope carries a stable `id` (telegram `message_id`, wacli id, or minted)
and a `thread_id` (telegram chat id / whatsapp group JID). To answer *that* message pass
its `id` as `reply_to` — telegram uses `reply_to_message_id`, whatsapp uses wacli
`--reply-to`, webhook echoes it. `/send` returns the id of YOUR outbound message, so
consumers can thread onto their own sends too.

**Conversation-first shortcut:** `reply_to: "last"` (API) or `--reply-to last` (CLI)
answers the newest inbound message on the channel (scoped to `to`'s thread when given)
and inherits its thread — "reply to the obvious previous message" with zero id
bookkeeping: `messenger send --channel ops --text "on it" --reply-to last`.

## Attachments

Envelopes carry media. Inbound files are downloaded to `$MESSENGER_HOME/media` and ride
as `attachments` (type/name/mime/path/url/size); consumers fetch the bytes via
`GET /media/<basename>`. Outbound is one flag — `messenger send --channel ops --file
report.pdf [--text "caption"]` (or `/send` with `file`/`attachments`); a URL is fetched
by the platform.

## Secrets

Referenced by **NAME** only (`TELEGRAM_BOT_TOKEN`, `MESSENGER_HOOK_SECRET`,
`MESSENGER_SERVE_TOKEN`). A value lives only in the process environment or the age
vault, never in `config.toml`, code, logs, or output. WhatsApp shells `wacli` (a paired
WhatsApp-Web device) — no whatsmeow link-in.

## Agent skill

`skills/messenger/SKILL.md` is a portable front door, embedded in the binary. The
binary installs its own skills — no scripts: `messenger install --skills` (add `--dir`
to target another skill directory) so an agent can drive messenger by intent.
