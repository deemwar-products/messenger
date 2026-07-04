# messenger

Standalone, **broker-free channel I/O** as one static Go binary. It owns receiving and
sending messages over **telegram**, **whatsapp**, and a generic **HMAC hook**, so any
product (a trading desk, an agent OS, a script) consumes it over plain HTTP — no broker to
run. Every message is a self-describing **Envelope**, so replies **thread** to a specific
message.

```
inbound: telegram/whatsapp/hook → Envelope {id,channel,from,thread_id,reply_to,text,ts}
         → inbox.ndjson (+ optional webhook push)
outbound: send(channel, text, --to thread, --reply-to id) → matching adapter → delivered
```

## Quick start

```sh
task setup                 # scaffold ~/.config/messenger/config.toml + print secret NAMES
cp .env.example .env       # fill in the secret VALUES (gitignored, never committed)
task serve                 # channel webhooks + POST /send, GET /inbox, GET /health
```

Everything is **task-driven** and env comes from `.env` via the Taskfile — never the
ambient shell. `task -l` lists all verbs.

## Verbs

| verb | what |
|------|------|
| `task setup` | scaffold config + home; print the secret NAMES to export |
| `task serve` | HTTP server: `/health`, `POST /send`, `GET /inbox?since=N`, + channel webhooks |
| `task listen` | ingress only: append inbound to the inbox; optional `--webhook URL` push |
| `task send -- --channel telegram --text "hi" --to 123 --reply-to 42` | one-shot egress, threaded |
| `task install-skill` | symlink the agent skill into `~/.claude/skills` |

## HTTP API

- `POST /send` — `{channel, text, to?, reply_to?}` (bearer-auth when `serveTokenEnv` set)
- `GET /inbox?since=N` — `{messages, next}`; `N` is a 1-based offset, pass `next` back
- `GET /health` — `{ok, channels}`

## Threaded replies

Each inbound Envelope carries a stable `id` (telegram `message_id`, wacli id, or minted)
and a `thread_id`. To answer *that* message, pass its `id` as `reply_to` — telegram uses
`reply_to_message_id`, whatsapp uses wacli `--quote`, hook echoes `reply_to` to the callback.

## Secrets

Referenced by **NAME** only (`TELEGRAM_BOT_TOKEN`, `MESSENGER_HOOK_SECRET`,
`MESSENGER_SERVE_TOKEN`). A value lives only in `.env`/vault, never in `config.toml`, code,
logs, or output. WhatsApp shells `wacli` (a paired WhatsApp-Web device) — no whatsmeow link-in.

## Agent skill

`skills/messenger/SKILL.md` is a portable front door: `task install-skill` symlinks it into
`~/.claude/skills` so an agent can drive messenger by intent. Restart the session to pick it up.
