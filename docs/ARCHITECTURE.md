# messenger — architecture

One static Go binary (`CGO_ENABLED=0`, pure-Go deps), broker-free. This document is the
code-level companion to `SPEC.md` (the product model) and `API.md` (the HTTP surface).

## Big picture

```
                 ┌────────────────────────── messenger serve (ONE per host) ─────────────────────────┐
                 │                                                                                    │
 telegram POSTs ─┼→ /telegram/<name> ──┐                                                             │
 signed callers ─┼→ /webhook/<name> ───┤  Publisher                    ┌→ subscription "factory"     │
                 │                     ├──(fan-out)──→ inbox.ndjson ───┤   (own cursor, push+retry)  │
 wacli stream ←──┼── whatsapp Streamer ┘                (the queue)    └→ subscription "desk" …      │
 (ONE subprocess)│                                                                                    │
                 │  consumer API: POST /send · GET /inbox?since=N · GET /health                       │
                 └────────────────────────────────────────────────────────────────────────────────────┘
```

- **Inbound** is normalized to ONE value (`envelope.Envelope`) at the edge; everything
  downstream (inbox, subscriptions, consumers) speaks only envelopes.
- **The inbox file IS the queue.** Append-only NDJSON; subscription cursors are 1-based
  line offsets into it. No broker process, no fsync ceremony, trivially inspectable.
- **Outbound** routes by channel NAME through the same per-kind types that handle
  inbound — one implementation per kind, not two.

## Packages (dependency order)

| package | responsibility | key types |
|---|---|---|
| `envelope` | the ONE wire value + normalize/reply helpers | `Envelope`, `Attachment`, `Normalize`, `Inbound`, `Reply` |
| `home` | resolve `$MESSENGER_HOME` (default `~/.config/messenger`) | `Dir`, `ConfigPath`, `InboxPath` |
| `config` | TOML at rest: channels + subscriptions + secret NAMES | `Config`, `Transport`, `Subscription` |
| `inbox` | append-only NDJSON store, offset reads, newest-lookup | `Inbox.Append/Since/Last` |
| `channel` | the kind layer: polymorphic `Kind` registry, `Channel` interface, runtime, vault | `Kind`, `Base`, `Channel`, `Pushed`, `Streamer`, `Streaming`, `Runtime`, `SecretResolver` |
| `subscription` | durable consumer push over the inbox | `Dispatcher` |
| `server` | the HTTP surface for `serve` | `Server` |
| `skills` | go:embed'd agent skill, installed by the binary | `Install` |
| `cmd/messenger` | the CLI — the whole product interface | verbs |

No package imports upward; `channel` knows nothing about HTTP serving or subscriptions.

## The channel layer (the core abstraction)

Two polymorphic types, one per concern. A **`Channel`** is one configured endpoint
(ingress + egress on the same value); a **`Kind`** is the whole personality of a kind —
its wire behavior AND its CLI behavior — so `main.go` dispatches with **zero kind
conditionals**.

```go
type Channel interface {                    // one instance per configured channel
    Name() string; Kind() string
    Send(ctx, env Envelope) (providerID string, err error)
}
type Pushed   interface { Channel; Path() string; Handler(pub Publisher) http.Handler }
type Streamer interface { Run(ctx, pub Publisher) error }

type Kind interface {                        // one concrete type per kind, embeds Base
    Name() string; Traits() Traits           // {RequiresToken, TargetFlag} — declarative facts
    Open(name, cfg, resolver) (Channel, error)
    Validate / AddHints / Connect / Test / Detail / Lane / Status   // the CLI surface
}
type Streaming interface {                   // capability: ONE shared inbound stream per kind
    OpenStream(allChannelsOfKind, resolver) (Streamer, error)
}
type Base struct{}                           // neutral defaults; a kind overrides only what it has
```

Kinds live in an **open registry** (`channel.Register`); each built-in registers from
its OWN file's `init()` (`whatsapp.go`, `telegram.go`, `webhook.go`). Adding a kind
(teams, slack, …) = one file: a struct embedding `Base`, its `Name/Traits/Open` plus
whichever hooks it has, and one `Register` call. The CLI, runtime, `/send`,
subscriptions, and threading need zero edits — every verb calls the kind polymorphically
(`k.Connect(...)`, `k.Detail(...)`, `k.Lane(...)`).

### Two polymorphism seams, one idiom

The system's rule is *a thing is a type; a capability is an interface it may also
satisfy*. There are exactly two capability assertions, both structural:

- the runtime asserts `Channel.(Pushed)` — does this channel mount an HTTP handler?
- the runtime asserts `Kind.(Streaming)` — does this kind have ONE shared inbound
  stream? whatsapp does (the single wacli subprocess serving every group channel);
  telegram/webhook don't (each channel is `Pushed`).

The whatsapp stream is why `Streaming` exists: the paired device is host-global, so N
channels must NOT mean N subprocesses. `Runtime.Up` groups streaming kinds and opens
exactly ONE stream per kind, supervised with exponential backoff (reset after a healthy
minute).

### Per-kind behavior lives in the kind's file

| kind | config unit | inbound shape | notes |
|---|---|---|---|
| telegram | many; each channel its OWN bot | `Pushed` at `/telegram/<name>` | own token by NAME, default chat id |
| whatsapp | many GROUP channels, `Streaming` | ONE `Streamer` for the whole kind | one wacli subprocess; routes chat JID → channel name; no-group channel = catch-all; `Lane`/`Connect` enforce one-JID-one-channel and list FREE groups |
| webhook | many; each hook its own channel | `Pushed` at `/webhook/<name>` | HMAC in, optional signed callback out |

### Runtime

`channel.Runtime` owns the built channels: mounts `Pushed` handlers on one mux,
supervises streams, and routes `Send(env)` by `env.Channel`. Per-channel failure is
isolated (`errors.Join`, never fatal to siblings). `server` composes the runtime's mux
under its own (`/health`, `/send`, `/inbox` shadow it).

## Data flow

**Inbound:** platform edge (HTTP handler or stream line) → normalize to Envelope
(stable `id` = platform message id when available; `thread_id` = chat/group; `channel` =
configured NAME) → `Publisher` → fan-out: inbox.Append + Dispatcher.Notify. Media is
downloaded at the edge too — into `$MESSENGER_HOME/media`, referenced as
`attachments[].path` (served at `GET /media/<basename>`); a failed download never
blocks publish (the attachment rides metadata-only).

**Delivery (subscriptions):** one goroutine per enabled subscription. Loop: read cursor
file → `inbox.Since(cursor)` → filter by channel names (a filtered-out message still
advances the cursor) → POST each in order (HMAC-signed when `secretEnv` set) → persist
cursor after EACH 2xx. Failure = backoff 1s→60s and retry the SAME message.
At-least-once; consumers dedupe by envelope `id`. Wakes on notify + a 5s tick.

**Outbound:** `/send` or CLI `send` → resolve `reply_to:"last"` via `inbox.Last(channel,
thread)` (inherits the thread) → `Runtime.Send` → kind's `Send` → provider message id
returned to the caller (telegram `result.message_id`, wacli send id, else the minted
envelope id). Each kind maps an attachment to its platform's upload (telegram
sendPhoto/…/sendDocument by type, whatsapp `wacli send file`, webhook passes the
envelope through); `text` rides as the caption, and a `url` attachment is fetched by
the platform itself.

## Design decisions (and why)

1. **Broker-free.** The consumers are few and local-ish; an append-only file + cursors
   gives durability and replay without operating a broker. The file doubles as the
   audit log (`GET /inbox` is the debug surface).
2. **One polymorphic type per kind, wire + CLI together.** First the old split
   (Connection registry + Sender registry) let the two halves drift; then a fat
   `KindSpec` struct-of-funcs still scattered each kind across loose functions and made
   every optional hook a nil-check. A `Kind` interface with a `Base` default puts a
   kind's ENTIRE personality — stream, sends, connectivity probe, pairing wizard, JID
   rules — in one file, and deletes every kind conditional from `main.go`.
3. **Single hub per host.** Telegram allows one webhook per bot; wacli allows one
   stream per device. `serve`/`listen` probe the addr (`/health` →
   `"service":"messenger"`) and reuse instead of double-binding.
4. **Secrets by NAME.** Config/CLI/logs carry env-var or age-vault NAMES only;
   `SecretResolver` reveals a value host-side at the exact call site (telegram URL,
   HMAC computation). Values never enter envelopes, config, or output.
5. **Conversation-first.** `id` + `thread_id` ride every envelope; `reply_to:"last"`
   makes "answer the previous message" a one-flag operation.
6. **The CLI is the interface.** No Taskfile, no install scripts; even the agent skill
   ships inside the binary (`install --skills`, go:embed).
7. **Module-path stability.** `github.com/deemwar-products/messenger`; private repo,
   consumed over HTTP, not as a library — packages may change freely.

## On-disk layout (`$MESSENGER_HOME`)

```
config.toml        channels + subscriptions + serveTokenEnv (0600; NAMES only)
inbox.ndjson       every inbound envelope, append-only (the queue + audit log)
media/             inbound media store — attachment `path` targets, served at GET /media/<file>
cursors/<name>     one integer per subscription (1-based line offset)
vault/<name>.age   optional age-encrypted secrets; keys/vault.key = host identity
```

## Testing seams

- `Publisher` is injected — channel tests capture envelopes with a closure, no sinks.
- whatsapp's `commandContext` is injected — stream tests run without wacli.
- telegram/webhook take `--option baseURL/callbackURL` — tests point at `httptest`.
- Subscription backoff/tick are fields — tests tighten them to milliseconds.
- Green gate: `CGO_ENABLED=0 go build ./... && go vet ./... && CGO_ENABLED=0 go test ./...`
