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
| `envelope` | the ONE wire value + normalize/reply helpers | `Envelope`, `Normalize`, `Inbound`, `Reply` |
| `home` | resolve `$MESSENGER_HOME` (default `~/.config/messenger`) | `Dir`, `ConfigPath`, `InboxPath` |
| `config` | TOML at rest: channels + subscriptions + secret NAMES | `Config`, `Transport`, `Subscription` |
| `inbox` | append-only NDJSON store, offset reads, newest-lookup | `Inbox.Append/Since/Last` |
| `channel` | the kind layer: ONE interface, registry, runtime, vault | `Channel`, `Pushed`, `Streamer`, `KindSpec`, `Runtime`, `SecretResolver` |
| `subscription` | durable consumer push over the inbox | `Dispatcher` |
| `server` | the HTTP surface for `serve` | `Server` |
| `skills` | go:embed'd agent skill, installed by the binary | `Install` |
| `cmd/messenger` | the CLI — the whole product interface | verbs |

No package imports upward; `channel` knows nothing about HTTP serving or subscriptions.

## The channel layer (the core abstraction)

```go
type Channel interface {                    // ONE type per kind: ingress + egress together
    Name() string; Kind() string
    Send(ctx, env Envelope) (providerID string, err error)
}
type Pushed   interface { Channel; Path() string; Handler(pub Publisher) http.Handler }
type Streamer interface { Run(ctx, pub Publisher) error }   // kind-LEVEL shared stream

type KindSpec struct {
    Name string; Shared bool; RequiresToken bool; TargetFlag string
    Open       func(name, cfg, resolver) (Channel, error)
    OpenStream func(allChannelsOfKind, resolver) (Streamer, error)  // Shared kinds only
    Test       func(ctx, name, cfg, resolver) ([]string, error)     // connectivity probe
}
```

Kinds live in an **open registry** (`channel.Register`); built-ins register in `init()`.
Adding a kind (teams, slack, …) = one file implementing `Channel` (+ `Pushed` or a
`Streamer`) + one `Register` call. The CLI, runtime, `/send`, subscriptions, and
threading need zero edits — `channel add/connect/test` read their behavior off the spec.

### Per-kind multiplicity (why KindSpec exists)

| kind | config unit | inbound shape | notes |
|---|---|---|---|
| telegram | many; each channel its OWN bot | `Pushed` at `/telegram/<name>` | own token by NAME, default chat id |
| whatsapp | many GROUP channels, **Shared=true** | ONE `Streamer` for the whole kind | one wacli subprocess; routes chat JID → channel name; no-group channel = catch-all |
| webhook | many; each hook its own channel | `Pushed` at `/webhook/<name>` | HMAC in, optional signed callback out |

The whatsapp stream is the reason `Shared`/`OpenStream` exist: the paired device is
host-global, so N channels must NOT mean N subprocesses. `Runtime.Up` groups shared
kinds and opens exactly ONE stream per kind, supervised with exponential backoff
(reset after a healthy minute).

### Runtime

`channel.Runtime` owns the built channels: mounts `Pushed` handlers on one mux,
supervises streams, and routes `Send(env)` by `env.Channel`. Per-channel failure is
isolated (`errors.Join`, never fatal to siblings). `server` composes the runtime's mux
under its own (`/health`, `/send`, `/inbox` shadow it).

## Data flow

**Inbound:** platform edge (HTTP handler or stream line) → normalize to Envelope
(stable `id` = platform message id when available; `thread_id` = chat/group; `channel` =
configured NAME) → `Publisher` → fan-out: inbox.Append + Dispatcher.Notify.

**Delivery (subscriptions):** one goroutine per enabled subscription. Loop: read cursor
file → `inbox.Since(cursor)` → filter by channel names (a filtered-out message still
advances the cursor) → POST each in order (HMAC-signed when `secretEnv` set) → persist
cursor after EACH 2xx. Failure = backoff 1s→60s and retry the SAME message.
At-least-once; consumers dedupe by envelope `id`. Wakes on notify + a 5s tick.

**Outbound:** `/send` or CLI `send` → resolve `reply_to:"last"` via `inbox.Last(channel,
thread)` (inherits the thread) → `Runtime.Send` → kind's `Send` → provider message id
returned to the caller (telegram `result.message_id`, wacli send id, else the minted
envelope id).

## Design decisions (and why)

1. **Broker-free.** The consumers are few and local-ish; an append-only file + cursors
   gives durability and replay without operating a broker. The file doubles as the
   audit log (`GET /inbox` is the debug surface).
2. **One interface per kind, ingress+egress together.** The old split (Connection
   registry + Sender registry) let the two halves drift and compiled even when a kind
   existed in only one. One type per kind makes that unrepresentable.
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
cursors/<name>     one integer per subscription (1-based line offset)
vault/<name>.age   optional age-encrypted secrets; keys/vault.key = host identity
```

## Testing seams

- `Publisher` is injected — channel tests capture envelopes with a closure, no sinks.
- whatsapp's `commandContext` is injected — stream tests run without wacli.
- telegram/webhook take `--option baseURL/callbackURL` — tests point at `httptest`.
- Subscription backoff/tick are fields — tests tighten them to milliseconds.
- Green gate: `CGO_ENABLED=0 go build ./... && go vet ./... && CGO_ENABLED=0 go test ./...`
