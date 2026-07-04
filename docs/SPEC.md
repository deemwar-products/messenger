# messenger v2 — conversation hub (spec)

messenger is the shared front door for channel I/O: one static Go binary
(CGO_ENABLED=0) that owns receiving and sending over **telegram**, **whatsapp**,
and generic **webhook** channels, and delivers inbound messages to N consumers
(the-factory, crypto-desk, scripts) with durable per-consumer cursors. No broker.
The CLI is the interface — no Taskfile.

## Core model

```
ports (channels)          hub                     consumers (subscriptions)
telegram bot A ─┐                                  ┌─> factory   POST url (cursor: 41)
telegram bot B ─┤→ Envelope → inbox.ndjson ──────→ ├─> cryptodesk POST url (cursor: 40)
whatsapp device ┤   (append-only, the queue)       └─> ad-hoc: GET /inbox?since=N
webhook hooks  ─┘
                 ← POST /send {channel,text,to,reply_to} → returns provider msg id
```

- **Envelope** is the one wire value: `{id, channel, account, sender, text,
  origin, thread_id, reply_to, ts, meta}`. `id` is stable (telegram message_id /
  wacli id / minted); replies thread via `reply_to`.
- **Channels are ports**, keyed by NAME in config. Messenger owns transport +
  delivery guarantees; consumers own meaning (no routing rules in messenger).

## Channel kinds — one common interface, per-kind multiplicity

```go
type Channel interface {          // one type per kind: telegram / whatsapp / webhook
    Name() string; Kind() string
    Send(ctx, env Envelope) (providerID string, err error)
}
type Pushed   interface { Channel; Path() string; Handler(pub Publisher) http.Handler }
type Streamer interface { Run(ctx, pub Publisher) error }   // kind-level shared stream

type KindSpec struct {
    Name          string
    Shared        bool   // ONE runtime stream serves ALL channels of this kind
    RequiresToken bool
    Open          func(name, cfg, resolver) (Channel, error)
    OpenStream    func(allChannelsOfKind, resolver) (Streamer, error) // nil = pushed-only
}
```

| kind | config unit | inbound | outbound target | secret |
|------|-------------|---------|-----------------|--------|
| `telegram` | many — each channel = its own bot | Bot API webhook `/telegram/<name>` | chat id (`--chat-id` default) | bot token by NAME |
| `whatsapp` | many named GROUP channels sharing ONE global paired device | ONE `wacli --json sync --follow` stream, routed to the channel whose `group` JID matches the chat | group JID (`--group` = the channel's group; a channel with no group = device catch-all) | none (wacli owns the session) |
| `webhook` | many — each hook = its own channel | HMAC-signed POST `/webhook/<name>` | `callbackURL` option | HMAC secret by NAME |

WhatsApp is **global**: the paired device belongs to the host, not to a channel.
Adding a second whatsapp channel never re-pairs and never spawns a second
stream. `channel connect` on any whatsapp channel checks `wacli doctor --json` —
already `authenticated` → report the linked JID + list groups (pick a JID for
`--group`); not authenticated → run `wacli auth` (QR) once for all.

Kind `hook` is accepted as a legacy alias for `webhook` on load.

## Subscriptions — durable, at-least-once, per-consumer cursor

`[subscriptions.<name>]` in config: `url`, `channels` (empty = all), `secretEnv`
(optional — push is HMAC-signed `X-Messenger-Signature-256`), `enabled`.

The dispatcher (runs inside `serve`/`listen`) keeps one goroutine per
subscription: read cursor from `$MESSENGER_HOME/cursors/<name>`, `inbox.Since`,
filter by channel names, POST each envelope in order; advance + persist the
cursor only on 2xx; on failure back off (1s → 60s) and retry — a consumer that
was down catches up from its cursor. Wakes on new-append notify + 5s tick.
`--webhook URL` on `listen` remains as an ad-hoc unnamed push (no cursor).

## Send returns the provider message id

`POST /send` and `messenger send` return the id the platform assigned
(telegram `result.message_id`, wacli send id when available, else the minted
envelope id) so consumers can thread onto their own outbound messages.

## CLI (the interface — wizard-grade, no Taskfile)

```
messenger setup                              scaffold home + empty config, then guide
messenger status                             config + channels + whatsapp device state + inbox size
messenger channel add telegram <name> --token-env NAME [--chat-id ID]
messenger channel add whatsapp <name> [--group <jid>]      # device is global
messenger channel add webhook  <name> --token-env NAME
messenger channel list | remove <name> | connect <name> [--public-url URL]
messenger subscribe add <name> --url URL [--channels a,b] [--secret-env NAME]
messenger subscribe list | remove <name>
messenger listen [--addr :14310] [--webhook URL]   ingress + dispatcher, no consumer API
messenger serve  [--addr :14310]                   everything on one port
messenger send --channel <name> --text T [--to THREAD] [--reply-to ID]
```

Wizard behavior: every verb prints the next step. `channel add whatsapp` checks
wacli + pairing and says exactly what to do; `connect` is idempotent; `status`
is the one-glance health view.

## Invariants

- Single static binary, CGO_ENABLED=0, pure-Go deps only.
- Secrets by NAME only (env var or age vault entry), resolved host-only at the
  point of use; a value never enters config, logs, output, or the envelope.
- `POST /send` + `GET /inbox` bearer-auth'd when `serveTokenEnv` is set.
- Inbound webhooks carry their own per-channel HMAC/secret.
- Per-channel/stream failure is isolated and supervised with backoff.
