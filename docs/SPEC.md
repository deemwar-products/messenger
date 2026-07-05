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
  origin, thread_id, reply_to, ts, meta, attachments}`. `id` is stable (telegram
  message_id / wacli id / minted); replies thread via `reply_to`; media rides as
  `attachments` (see below).
- **Channels are ports**, keyed by NAME in config. Messenger owns transport +
  delivery guarantees; consumers own meaning (no routing rules in messenger).

## Channel kinds — one common interface, per-kind multiplicity

```go
type Channel interface {          // one instance per configured channel
    Name() string; Kind() string
    Send(ctx, env Envelope) (providerID string, err error)
}
type Pushed   interface { Channel; Path() string; Handler(pub Publisher) http.Handler }
type Streamer interface { Run(ctx, pub Publisher) error }

// Kind is the polymorphic face of one kind — wire AND CLI behavior on one type,
// so main.go dispatches with zero kind conditionals. Base supplies neutral defaults.
type Kind interface {
    Name() string; Traits() Traits                    // {RequiresToken, TargetFlag}
    Open(name, cfg, resolver) (Channel, error)
    Validate(name, cfg, existing) error               // add-time rules (wa: 1 JID = 1 channel)
    AddHints(name, cfg) []string                      // post-add guidance
    Connect(name, cfg, ConnectParams) error           // pair/register (wa: QR + FREE-group list)
    Test(ctx, name, cfg, resolver) ([]string, error)  // connectivity probe
    Detail(name, cfg) string                          // `channel list` target column
    Lane(name, LaneParams, existing) (Transport, hints, error)  // `register` agent lanes
    Status() []string                                 // host-level state (wa: device)
}
type Streaming interface {                            // capability: ONE shared stream per kind
    OpenStream(allChannelsOfKind, resolver) (Streamer, error)
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

## Conversation is first-class

Every inbound envelope carries `id` + `thread_id`; a reply is `{channel, text,
reply_to: <id>}`. The shortcut `reply_to: "last"` resolves to the newest inbound
message on the channel (scoped to `to`'s thread when given) and inherits its thread —
the "answer the obvious previous message" case needs no id bookkeeping. Subscriptions
default to ALL channels (the `channels` filter is opt-in), so a consumer holds whole
conversations across every port.

## Attachments — store and reference

Media is store-and-reference: `$MESSENGER_HOME/media` is the store, the envelope
carries only metadata plus a `path` into it —
`attachments: [{type, name, mime, path, url, size}]`, `type` ∈
image|video|audio|voice|document|file. Inbound media is downloaded at the edge (per
kind) into the media dir; `GET /media/<basename>` serves the bytes to remote
consumers (bearer-auth'd like `/inbox`, path-traversal-safe). A failed download never
blocks publish — the attachment rides metadata-only. Outbound: `--file PATH|URL`
(CLI) or `{"file": …}` / a full `attachments` array (`/send`); `text` becomes the
caption; a `url` attachment is fetched by the platform.

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
messenger send --channel <name> [--text T] [--file PATH|URL] [--to THREAD] [--reply-to ID]
```

Wizard behavior: every verb prints the next step. `channel add whatsapp` checks
wacli + pairing and says exactly what to do; `connect` is idempotent; `status`
is the one-glance health view.

## Single instance per host

One hub owns the host's channels: a second `serve` would split telegram webhook
delivery and spawn a competing wacli stream. `serve`/`listen` probe the address first
(`GET /health` → `{"service":"messenger"}`) and reuse a running instance instead of
double-starting. Multiple installs/agents all talk to the one hub over HTTP.

## Extending with new kinds (teams, slack, …)

A new kind is ONE file in `channel/`: a struct embedding `channel.Base` that implements
`Name`, `Traits`, `Open` (plus `Streaming` when its inbound is one shared stream, and
whichever CLI hooks it has — `Validate/AddHints/Connect/Test/Detail/Lane/Status`), and
one `channel.Register(teamsKind{})` in its `init()`. The CLI (`channel
add/list/connect/test`, `register`), the runtime supervision, `/send`, subscriptions,
and threading all work with ZERO edits elsewhere — main.go contains no kind
conditionals at all.

## Invariants

- Single static binary, CGO_ENABLED=0, pure-Go deps only.
- Secrets by NAME only (env var or age vault entry), resolved host-only at the
  point of use; a value never enters config, logs, output, or the envelope.
- `POST /send` + `GET /inbox` bearer-auth'd when `serveTokenEnv` is set.
- Inbound webhooks carry their own per-channel HMAC/secret.
- Per-channel/stream failure is isolated and supervised with backoff.
