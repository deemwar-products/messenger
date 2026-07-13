# The Universal Hook — reach this hub, both directions

`messenger` is broker-free. The **universal hook** makes one hub *itself* addressable as a
hook: any other messenger instance, agent, or process — on any host — reaches it through a
single, symmetric, HMAC-signed endpoint pair, for every channel, with no per-peer server
config.

> Owner principle: *"messenger itself should have a hook for send/receive so other
> messengers can reach it."*

This is **additive**. The existing consumer API is unchanged: bearer-auth `POST /send`,
`GET /inbox?since=N`, `GET /media/<basename>`, and the per-lane HMAC webhooks
`POST /webhook/<name>` (e.g. `/webhook/hook`, `/webhook/ceo`) all keep working exactly as
before. The universal hook is the *single documented "reach me" contract* layered on top.

---

## The contract

| Endpoint | Direction | Auth | Purpose |
|----------|-----------|------|---------|
| `POST /hook/send` | peer → hub (IN)  | HMAC `X-Hub-Signature-256` | Push a message into any channel/lane; optionally route it OUT. |
| `POST /hook/recv` | hub → peer (OUT) | HMAC `X-Hub-Signature-256` | Poll inbound messages since a cursor, filtered by channel. |

**One shared secret, both directions.** Both endpoints verify an HMAC-SHA256 signature over
the raw request body, in the header `X-Hub-Signature-256`, value `sha256=<hex>`, keyed by a
secret referenced **by NAME only**: `MESSENGER_HOOK_SECRET` (override the env NAME via
config `hookSecretEnv`). The value lives in the host environment; it never enters config, a
log, or a response. If the env var is unset, both `/hook/*` endpoints return `503` — a peer
can only reach the hub once the owner sets the secret.

This is the **same** signing primitive (`sha256=<hex>` HMAC in `X-Hub-Signature-256`) the
per-lane webhook channel already uses inbound *and* outbound — so two messengers are
symmetric: what one hub signs on delivery, the other verifies on receive, with no header or
scheme translation.

### Why poll for OUT (and how to get push instead)

**Design fork — resolved:** `/hook/recv` is a **poll**, not a push-callback. Poll is
stateless on the hub, needs no per-peer registration or durable cursor server-side, and the
peer owns its own cursor — the simplest thing that is symmetric and idempotent.

A peer that wants **push** delivery instead registers a durable subscription callbackURL —
that machinery already exists (`subscription/`), so the hook does not duplicate it:

```sh
messenger register <peer-name> --url https://<peer-host>/hook/send --secret-env SHARED_SECRET_ENV
# optionally: --group/--channels to filter which lanes are pushed
```

The hub then POSTs every matching inbound envelope to the peer's URL, in order, retrying
with a per-consumer cursor until the peer acks `2xx`. (Note: subscription push signs with
`X-Messenger-Signature-256`; the poll and per-lane webhook paths use `X-Hub-Signature-256`.
Same HMAC primitive, different header name — a peer verifying push must read the former.)

---

## Payload schema

### `POST /hook/send` (IN)

Body is JSON. The **message** fields are the same lenient shape as a per-lane webhook, plus
two hub-routing controls:

| field | type | meaning |
|-------|------|---------|
| `channel` | string | Lane the message belongs to. Default `"hook"`. With `deliver:true`, the channel it is sent OUT through. |
| `deliver` | bool | `false`/absent = inject inbound only (into inbox + subscriptions). `true` = route OUT through `channel` like `/send`. |
| `text` *(or `message`)* | string | Message body. Falls back to the raw request body if absent. |
| `sender` *(or `login`)* | string | Who sent it. |
| `id` | string | Message id. **Supply it for idempotency** — consumers dedupe by `id` (at-least-once). |
| `thread_id` | string | Conversation/thread key. |
| `reply_to` | string | Message id this threads onto. |
| `attachments[]` | array | `{type,name,mime,url,size}`. **`url` only** — a remote `path` is never trusted; the hub sets local `path` for media *it* stores. See *Attachments*. |

Response: `202 {"ok":true,"id":"<id>"}` for an inbound inject, or
`200 {"ok":true,"id":"<provider-id>","delivered":true}` when `deliver:true` routed it out.

### `POST /hook/recv` (OUT)

| field | type | meaning |
|-------|------|---------|
| `since` | int | 1-based cursor. Return messages *after* this offset. Persist `next` and pass it back next poll. |
| `channels` | []string | Optional filter — return only these channels. Empty = all. |

Response: `200 {"ok":true,"messages":[<envelope>...],"next":<int>}`.

An envelope carries `id, channel, account, sender, text, origin, thread_id, reply_to, ts,
meta, attachments[]`.

---

## Attachments

There is **one media path**. On inbound (`/hook/send`), attachments pass **by reference**:
only `url` is honored — a peer's `path` is ignored, because `path` names a local file the
hub itself stored. To send a file, the peer hosts it and passes a fetchable `url` (commonly
the peer hub's own `GET /media/<basename>`, fetched with the peer's serve token, or any
reachable URL).

On delivery OUT (`deliver:true` or a subscription push), the envelope's `attachments[].url`
travels as-is. If you are forwarding media the sending hub stored, set
`url = http://<sending-hub>/media/<basename>` so the receiver can fetch it (that endpoint is
bearer-auth'd — the peer needs the sending hub's serve token, or mirror the bytes to a URL
it can already read).

---

## Examples (curl)

Shared secret is referenced by NAME; the value expands only inside the `openssl` call, never
printed:

```sh
HOST=http://127.0.0.1:14310

# --- Push a message IN (inbound inject) ---
BODY='{"channel":"hook","id":"peer-42","text":"hello from peer-A","sender":"peerbot"}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$MESSENGER_HOOK_SECRET" -hex | awk '{print $NF}')"
curl -sS -X POST "$HOST/hook/send" -H "X-Hub-Signature-256: $SIG" -d "$BODY"
# -> 202 {"ok":true,"id":"peer-42"}

# --- Push a message IN with an attachment, and route it OUT over a channel ---
BODY='{"channel":"cryptodesk","deliver":true,"text":"chart attached","attachments":[{"type":"image","name":"btc.png","mime":"image/png","url":"https://peer-A/media/btc.png"}]}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$MESSENGER_HOOK_SECRET" -hex | awk '{print $NF}')"
curl -sS -X POST "$HOST/hook/send" -H "X-Hub-Signature-256: $SIG" -d "$BODY"
# -> 200 {"ok":true,"id":"<provider-msg-id>","delivered":true}

# --- Poll messages OUT since your cursor, filtered to one channel ---
BODY='{"since":0,"channels":["hook"]}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$MESSENGER_HOOK_SECRET" -hex | awk '{print $NF}')"
curl -sS -X POST "$HOST/hook/recv" -H "X-Hub-Signature-256: $SIG" -d "$BODY"
# -> 200 {"ok":true,"messages":[...],"next":7}  # persist next, pass it back
```

---

## How another messenger reaches this hub (symmetric loop)

Hub **A** and hub **B** share one secret NAME (`MESSENGER_HOOK_SECRET`, same value both
hosts). Then, with no other config:

```
B → A:  B signs a body with the shared secret and POSTs A's  /hook/send   (A verifies, injects/routes)
A → B:  A signs a body with the shared secret and POSTs B's  /hook/send   (B verifies, injects/routes)
either side pulls the other's queue with  POST /hook/recv {since}         (same signature)
```

For **push** instead of poll on the receive side, register the peer as a subscription (or as
a `webhook` channel whose `callbackURL` is the peer's `/hook/send`) — the hub then delivers
outbound automatically, signed the same way.

---

## Notes / reserved names

- `/hook/send` and `/hook/recv` are exact routes and win over the `/hook/<name>` legacy
  webhook alias — so a `webhook` channel must **not** be named `send` or `recv`.
- `/hook/send` is the HMAC twin of bearer-auth `/send`; `/hook/recv` is the HMAC twin of
  bearer-auth `/inbox`. Use whichever auth the caller already holds.
- Idempotency is by envelope `id`, deduped by consumers (at-least-once), matching the rest
  of the hub.
