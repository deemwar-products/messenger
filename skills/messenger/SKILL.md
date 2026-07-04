---
name: messenger
description: >
  Drive the standalone `messenger` product — broker-free channel I/O over telegram,
  whatsapp, and a generic HMAC hook, with threaded replies. Trigger when the user says:
  "set up messenger", "run messenger", "listen for messages", "send a message on
  telegram/whatsapp", "reply to that message", "start the messenger server", "wire a
  channel", "post to /send", "poll the inbox". Use whenever a session needs to receive or
  send channel messages without standing up a broker.
---

# messenger — listen / send / serve (threaded, broker-free)

`messenger` is one static Go binary that owns channel I/O so anything can receive and
send messages over plain HTTP. No broker. Every inbound message is normalized to a
self-describing **Envelope** `{id, channel, from, thread_id, reply_to, text, ts}`, so a
reply threads to a specific message with no external lookup.

```
inbound: telegram/whatsapp/hook → normalize → Envelope → inbox.ndjson (+ optional webhook push)
outbound: send(channel, text, --to thread, --reply-to id) → matching adapter → delivered, threaded
```

## The verbs (always via `task`, env from .env — never the shell)

- **`task setup`** — scaffold `~/.config/messenger/config.toml` + home dirs (empty), then
  point at `channel add`. Secrets are referenced by NAME only; a value never enters config.
- **channels — many of any kind, keyed by name:**
  - `messenger channel add whatsapp home`
  - `messenger channel add telegram mybot --token-env TELEGRAM_BOT_TOKEN --chat-id -1001234567890`
  - `messenger channel add hook incoming --token-env MESSENGER_HOOK_SECRET`
  - `messenger channel list` — NAME / KIND / ENABLED / default target
  - `messenger channel remove <name>`
  - `messenger channel connect <name>` — whatsapp QR pair (`wacli auth`); telegram prints the
    `setWebhook` (add `--public-url https://host`). A configured `--chat-id` becomes the
    default send target, so `send --channel mybot --text hi` needs no `--to`.
- **`task serve`** — the small HTTP server: channel webhooks + the consumer API on one port.
  - `GET  /health`
  - `POST /send`  `{channel, text, to?, reply_to?}` (bearer-auth when `serveTokenEnv` set)
  - `GET  /inbox?since=N` → `{messages, next}` (N = 1-based line offset; pass `next` back)
- **`task listen`** — run just the ingress: append inbound to the inbox and optionally
  `--webhook URL` POST each Envelope to a subscriber (this is the clean replacement for a
  tail-and-forward relay).
- **`task send -- --channel telegram --text "hi" --to <thread> --reply-to <msgid>`** — one-shot egress.

## Threaded replies (the point)

Every inbound Envelope carries a stable `id` (telegram `message_id`, wacli id, or minted).
To reply *to that message*, pass its `id` as `reply_to`:
- telegram → `reply_to_message_id`
- whatsapp → wacli `--quote <id>`
- hook → `reply_to` field echoed to the callback

`GET /inbox` gives you each message's `id` and `thread_id`; feed them straight back into
`POST /send` to answer in-thread.

## Channels

`telegram` (Bot API webhook + send), `whatsapp` (shells `wacli`, a paired WhatsApp-Web
device — no whatsmeow link-in), `hook` (generic HMAC-signed inbound: any signed caller can
inject a message). Enable per channel in `config.toml`; add more by registering an adapter.

## Rules

- **Task-driven.** Never run `go run`/the binary directly in a way that bypasses `.env`.
  Use the `task` verbs so env loads from the Taskfile's `dotenv`, not the ambient shell.
- **Secrets are use-only.** Reference by env-var NAME (`TELEGRAM_BOT_TOKEN`,
  `MESSENGER_HOOK_SECRET`, `MESSENGER_SERVE_TOKEN`); never print, log, or bake a value.
- **A dedicated bot/number per consumer.** One Telegram bot / WhatsApp number has ONE
  consumer — give messenger its own, or bridge from an existing owner via the hook.
