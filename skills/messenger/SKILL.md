---
name: messenger
description: >
  Drive the standalone `messenger` product — a broker-free conversation hub over
  telegram, whatsapp, and webhook channels, with threaded replies and durable consumer
  subscriptions. Trigger when the user says: "set up messenger", "run messenger",
  "listen for messages", "send a message on telegram/whatsapp", "reply to that message",
  "start the messenger server", "wire a channel", "subscribe a consumer", "post to
  /send", "poll the inbox". Use whenever a session needs to receive or send channel
  messages without standing up a broker.
---

# messenger — the broker-free conversation hub (threaded, durable)

`messenger` is one static Go binary; the CLI is the whole interface. Every inbound
message is normalized to an **Envelope** `{id, channel, sender, text, thread_id,
reply_to, ts}`; consumers get it via durable push subscriptions or `GET /inbox`.

```
telegram bots / ONE whatsapp device (many groups) / webhooks
   → Envelope → inbox.ndjson → subscriptions (per-consumer cursor, at-least-once)
send(channel, text, --to thread, --reply-to id) → delivered, returns provider msg id
```

## Verbs

- **`messenger setup`** — scaffold `~/.config/messenger` + guided next steps (detects
  the global whatsapp device state). **`messenger status`** — one-glance health.
- **channels — kinds differ in multiplicity:**
  - `messenger channel add telegram <name> --token-env TELEGRAM_BOT_TOKEN --chat-id -100…`
    — many; each channel is its OWN bot.
  - `messenger channel add whatsapp <name> --group <group-jid>` — the paired device is
    GLOBAL (one per host, one shared stream); each channel is a GROUP. A channel with no
    `--group` is the catch-all for unmatched chats.
  - `messenger channel add webhook <name> --token-env MESSENGER_HOOK_SECRET` — many;
    each hook is its own channel at `/webhook/<name>` (HMAC `X-Hub-Signature-256`).
  - `messenger channel list | remove <name>`
  - `messenger channel connect <name>` — whatsapp: reports "already linked (<jid>)" +
    lists groups, or runs the ONE-time QR pair; telegram: prints the `setWebhook` call
    (`--public-url https://host`, token referenced by NAME only).
  - `messenger channel test [<name>]` — probe connectivity WITHOUT sending: whatsapp =
    device linked + group known; telegram = `getMe` (token by NAME, prints the bot
    username); webhook = secret resolvable. No name = test every channel.
- **subscriptions — durable consumer delivery:**
  - `messenger subscribe add <name> --url URL [--channels a,b] [--secret-env NAME]` —
    every envelope is POSTed in order; the consumer's cursor advances only on 2xx, so a
    down consumer catches up. `subscribe list | remove <name>`.
- **`messenger serve [--addr :14310]`** — everything on one port:
  - `GET  /health` · `POST /send` `{channel, text, to?, reply_to?}` → `{ok, id}`
    (bearer-auth when `serveTokenEnv` set) · `GET /inbox?since=N` → `{messages, next}`
  - plus the channel webhooks and the subscription dispatcher.
- **`messenger listen`** — ingress + subscriptions only (no consumer API).
- **`messenger send --channel <name> --text "hi" [--to <thread>] [--reply-to <msgid>]`**
  — one-shot egress; prints the provider-assigned message id.

## Threaded replies (the point)

Every inbound Envelope carries a stable `id` (telegram `message_id`, wacli id, or
minted) and a `thread_id` (telegram chat id / whatsapp group JID). To reply *to that
message*, pass its `id` as `--reply-to`: telegram → `reply_to_message_id`, whatsapp →
wacli `--quote`, webhook → echoed. `/send` returns YOUR message's id, so you can thread
onto your own sends. **Shortcut:** `--reply-to last` (or `"reply_to":"last"` on
`POST /send`) answers the newest inbound message on the channel — scoped to `--to`'s
thread when given, inheriting its thread otherwise — no id bookkeeping.

## Rules

- **ONE hub per host — always reuse.** Before starting anything, `messenger status`
  (or `GET /health` → `{"service":"messenger"}`). If a hub is running, talk to it over
  HTTP; never start a second `serve` — it would split telegram webhooks and whatsapp
  streams. `serve` itself refuses to double-start on the same addr.
- **Install this skill from any binary:** `messenger install --skills` (embedded copy,
  no repo checkout needed).

- **Secrets are use-only.** Reference by env-var NAME (`TELEGRAM_BOT_TOKEN`,
  `MESSENGER_HOOK_SECRET`, `MESSENGER_SERVE_TOKEN`); never print, log, or bake a value.
- **The whatsapp device is global.** Never re-pair a linked device; `channel connect`
  detects it. One wacli stream serves every group channel.
- **A dedicated bot per consumer on telegram.** One bot has ONE webhook; give messenger
  its own bot, or bridge from an existing owner via a webhook channel.
