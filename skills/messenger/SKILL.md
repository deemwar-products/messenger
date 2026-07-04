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

One static Go binary; the CLI is the whole interface (no Taskfile, no scripts). Every
inbound message is normalized to an **Envelope**:

```json
{"id":"420","channel":"ops","sender":"muthu","text":"deploy it",
 "thread_id":"120363408634625681@g.us","reply_to":"","ts":1783138037950}
```

`channel` = the configured channel NAME (not the kind). `id` + `thread_id` are all you
need to reply. Default port **:14310**; state in `$MESSENGER_HOME` (default
`~/.config/messenger`): `config.toml`, `inbox.ndjson`, `cursors/<consumer>`.

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

## Recipe 1 — first-time setup (nothing configured yet)

```sh
messenger setup                      # scaffolds config + prints device state
messenger channel add whatsapp ops --group 123456789@g.us
messenger channel add telegram mybot --token-env TELEGRAM_BOT_TOKEN --chat-id -1001234567890
messenger channel add webhook incoming --token-env MESSENGER_HOOK_SECRET
messenger channel connect ops        # whatsapp: "already linked (<jid>)" + group list, or one-time QR
messenger channel connect mybot --public-url https://host   # prints the setWebhook curl
messenger channel test               # probe everything WITHOUT sending; exits non-zero on failure
messenger serve &                    # the hub
```

Kind rules: **telegram** = many channels, each its OWN bot (own `--token-env`, default
`--chat-id`). **whatsapp** = ONE global paired device per host; each channel = a GROUP
(`--group <jid>`); a channel with no `--group` is the catch-all; never re-pair a linked
device — `connect` detects it and lists groups with their JIDs. **webhook** = many, each
its own HMAC path `/webhook/<name>`.

## Recipe 2 — send a message

```sh
messenger send --channel ops --text "build passed"            # to the channel's default target
messenger send --channel mybot --text "hi" --to 123456789     # explicit chat/thread
# or over HTTP against a running hub:
curl -sS -X POST http://127.0.0.1:14310/send \
  -H "Authorization: Bearer $MESSENGER_SERVE_TOKEN" -H "Content-Type: application/json" \
  -d '{"channel":"ops","text":"build passed"}'
# → {"ok":true,"id":"421"}   ← the PROVIDER message id; keep it to thread onto your own send
```

## Recipe 3 — reply to a message (threaded)

To answer a specific envelope, pass its `id` as `reply_to` (telegram →
`reply_to_message_id`, whatsapp → wacli `--quote`, webhook → echoed):

```sh
messenger send --channel ops --text "on it" --reply-to 420 --to 120363408634625681@g.us
```

To answer **the newest inbound message** (the usual conversational case), use `last` —
it resolves the id AND inherits the thread, zero bookkeeping:

```sh
messenger send --channel ops --text "on it" --reply-to last
curl … -d '{"channel":"ops","text":"on it","reply_to":"last"}'   # same over HTTP
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

## Recipe 5 — subscribe a consumer (durable push — prefer this over polling for services)

```sh
messenger subscribe add factory --url http://localhost:9000/hook            # ALL channels
messenger subscribe add desk --url http://localhost:9100/hook --channels ops --secret-env MY_PUSH_SECRET
messenger subscribe list                                                    # shows each cursor
```

Every envelope is POSTed to the URL in order. The consumer must answer 2xx; its cursor
(`$MESSENGER_HOME/cursors/<name>`) advances only on success, so a down consumer catches
up automatically (at-least-once — dedupe by envelope `id`). With `--secret-env`, verify
`X-Messenger-Signature-256` = `sha256=<hex hmac of body>`.

## Recipe 6 — inject a message from a script (inbound webhook)

```sh
body='{"text":"deploy done","sender":"ci","thread_id":"run-42"}'
sig="sha256=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$MESSENGER_HOOK_SECRET" -hex | awk '{print $NF}')"
curl -sS -X POST http://127.0.0.1:14310/webhook/incoming -H "X-Hub-Signature-256: $sig" -d "$body"
# → 202; the envelope now flows to the inbox + all subscriptions
```

## Recipe 7 — diagnose

```sh
messenger status          # config path, server running?, channels, device state, inbox size, cursors
messenger channel test    # per-channel connectivity: whatsapp device+group, telegram getMe, webhook secret
messenger channel list
```

## Rules

- **ONE hub per host — always reuse (Recipe 0).** Multiple installs/agents share it over HTTP.
- **Secrets are use-only.** Reference by env-var NAME (`TELEGRAM_BOT_TOKEN`,
  `MESSENGER_HOOK_SECRET`, `MESSENGER_SERVE_TOKEN`); never print, log, or bake a value.
- **The whatsapp device is global.** Never run `wacli auth` on a linked device; `channel
  connect` / `channel test` tell you the state.
- **A dedicated bot per consumer on telegram.** One bot has ONE webhook; give messenger
  its own bot, or bridge an existing owner in via a webhook channel.
- **This skill installs from any binary:** `messenger install --skills` (embedded copy).
