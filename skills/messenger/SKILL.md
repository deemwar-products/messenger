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

One static Go binary; the CLI is the whole interface. Every inbound message is
normalized to an **Envelope**:

```json
{"id":"420","channel":"ops","sender":"muthu","text":"deploy it",
 "thread_id":"120363408634625681@g.us","reply_to":"","ts":1783138037950}
```

`channel` = the configured channel NAME (not the kind). `id` + `thread_id` are all you
need to reply; media rides as `attachments` (send: Recipe 2, fetch: Recipe 4). Default
port **:14310**; state in `$MESSENGER_HOME` (default `~/.config/messenger`):
`config.toml`, `inbox.ndjson`, `media/`, `cursors/<consumer>`.

**Channel-kind specifics live in `references/` — read the one you're working with:**

- `references/whatsapp.md` — ONE global paired device, channels = groups, catch-all,
  pairing/JIDs, wacli gotchas
- `references/telegram.md` — one bot per channel, setWebhook, chat ids, getMe checks
- `references/webhook.md` — HMAC signing recipe, accepted body fields, callbacks

## Recipe 0 — ALWAYS first: is the hub already running?

```sh
curl -sS --max-time 2 http://127.0.0.1:14310/health
# → {"ok":true,"service":"messenger","channels":{"ops":"whatsapp","mybot":"telegram"}}
```

- `"service":"messenger"` → a hub is RUNNING. Use its HTTP API for everything below;
  NEVER start a second one (it would split telegram webhooks / whatsapp streams).
- Connection refused → no hub. Start one in the background: `messenger serve &`
  (it also self-guards: a second `serve` on the same addr just reports and exits 0).
- No binary? `which messenger` — if absent, build: `CGO_ENABLED=0 go build -o
  ~/.local/bin/messenger ./cmd/messenger` from the repo.

**Auth:** if `serveTokenEnv` is set in config.toml (default name
`MESSENGER_SERVE_TOKEN`), add `-H "Authorization: Bearer $MESSENGER_SERVE_TOKEN"` to
every `/send` and `/inbox` call. Reference the env var by NAME; never echo its value.

## Recipe 1 — first-time setup

```sh
messenger setup                      # scaffolds config + prints device state
messenger channel add <kind> <name> [flags]   # see references/<kind>.md for flags + model
messenger channel connect <name>     # pair/register (kind-specific; idempotent)
messenger channel test               # probe everything WITHOUT sending; non-zero on failure
messenger serve &                    # the hub
```

## Recipe 2 — send a message

```sh
messenger send --channel ops --text "build passed"            # to the channel's default target
messenger send --channel ops --text "hi" --to <thread-id>     # explicit thread/chat/group
messenger send --channel ops --file report.pdf --text "caption"   # attachment (--text optional)
# or over HTTP against a running hub:
curl -sS -X POST http://127.0.0.1:14310/send \
  -H "Authorization: Bearer $MESSENGER_SERVE_TOKEN" -H "Content-Type: application/json" \
  -d '{"channel":"ops","text":"build passed"}'
# → {"ok":true,"id":"421"}   ← the PROVIDER message id; keep it to thread onto your own send
```

`--file` / `"file"` takes a local path or an `http(s)` URL (the platform fetches URLs);
`text` becomes the caption and is optional when a file is given — text OR file is
required. Over HTTP, `{"channel":"ops","file":"/path/report.pdf"}` is the shorthand;
full control is `"attachments":[{type,name,mime,path,url,size}]`. Per-kind behavior:
`references/<kind>.md`.

## Recipe 3 — reply to a message (threaded)

To answer a specific envelope, pass its `id` as `reply_to`. To answer **the newest
inbound message** (the usual conversational case), use `last` — it resolves the id AND
inherits the thread, zero bookkeeping:

```sh
messenger send --channel ops --text "on it" --reply-to last
messenger send --channel ops --text "on it" --reply-to 420 --to <thread-id>   # explicit
curl … -d '{"channel":"ops","text":"on it","reply_to":"last"}'                # same over HTTP
```

`--to <thread>` with `last` scopes "newest" to that thread. 409/"no previous message"
means the conversation is empty — send without reply_to instead.

## Recipe 4 — read messages (poll)

```sh
curl -sS "http://127.0.0.1:14310/inbox?since=0" -H "Authorization: Bearer $MESSENGER_SERVE_TOKEN"
# → {"messages":[<envelopes>], "next": 7}
```

Loop: pass the returned `next` back as `since` on the following call; equal `next` =
nothing new. Persist `next` if you need to survive restarts. To answer something you
read, feed its `id`/`thread_id` straight into Recipe 3.

Envelopes may carry `attachments` (media metadata + a local `path` on the hub). Fetch
the bytes with the same bearer:

```sh
curl -sS "http://127.0.0.1:14310/media/<basename of attachments[].path>" \
  -H "Authorization: Bearer $MESSENGER_SERVE_TOKEN" -o file
```

## Recipe 5 — onboard an agent / subscribe a consumer (durable push — prefer over polling)

One-shot agent onboarding (lane + listen, idempotent — safe to re-run from boot scripts):

```sh
messenger register cryptodesk --group 123456789@g.us --url http://127.0.0.1:9100/hook
# whatsapp lane. Also: --kind telegram --token-env BOT_TOKEN [--chat-id ID], or
# --kind webhook --token-env HOOK_SECRET. Creates the channel (refuses a JID already
# bound elsewhere), registers subscription "cryptodesk" filtered to it, prints the agent's
# exact send/reply/listen contract. Omit --url → poll-mode instructions instead.
```

Plain subscriptions:

```sh
messenger subscribe add factory --url http://localhost:9000/hook            # ALL channels
messenger subscribe add desk --url http://localhost:9100/hook --channels ops --secret-env MY_PUSH_SECRET
messenger subscribe list                                                    # shows each cursor
```

Every envelope is POSTed to the URL in order. The consumer must answer 2xx; its cursor
(`$MESSENGER_HOME/cursors/<name>`) advances only on success, so a down consumer catches
up automatically (at-least-once — dedupe by envelope `id`). With `--secret-env`, verify
`X-Messenger-Signature-256` = `sha256=<hex hmac of body>`. Pushed envelopes include
`attachments` too — fetch local-path media via `GET /media/<basename>` (Recipe 4).

## Recipe 6 — inject a message from a script

Any local agent/script lands a message in the hub inbox through a webhook channel —
one verb, no HMAC dance:

```sh
messenger inject --channel incoming --text "deploy done" --sender ci --thread run-42
# → injected id=… channel=incoming    (202 from the running hub; flows to inbox + subscriptions)
```

The channel's secret is resolved by NAME (e.g. `$MESSENGER_HOOK_SECRET`) exactly as the
hub resolves it — export it in the calling shell; the value is never printed. `--reply-to
MSGID` threads, `--addr` reaches a non-default hub. Cross-host (no binary/config on the
caller)? The raw signed-curl recipe lives in `references/webhook.md`.

## Recipe 7 — diagnose

```sh
messenger status          # config path, server running?, channels, device state, inbox size, cursors
messenger channel test    # per-channel connectivity (see references/<kind>.md for failure modes)
messenger channel list
```

## Rules

- **ONE hub per host — always reuse (Recipe 0).** Multiple installs/agents share it over HTTP.
- **Secrets are use-only.** Reference by env-var NAME (`TELEGRAM_BOT_TOKEN`,
  `MESSENGER_HOOK_SECRET`, `MESSENGER_SERVE_TOKEN`); never print, log, or bake a value.
- **Read the kind's reference before touching its channel** — whatsapp/telegram/webhook
  have different multiplicity and pairing rules.
- **This skill installs from any binary:** `messenger install --skills` (embedded copy,
  references included).
