# messenger — HTTP API

Everything a consumer needs to integrate over HTTP against a running hub
(`messenger serve`, default `:14310`). The wire value everywhere is the Envelope.

## The Envelope

```json
{
  "id": "420",                              // stable per-message id (platform id or minted)
  "channel": "ops",                         // configured channel NAME (not the kind)
  "account": "",                            // optional platform account label
  "sender": "muthu",                        // who wrote it (username / user id / JID)
  "text": "deploy it",
  "origin": "WhatsApp",                     // producer: Telegram | WhatsApp | Webhook | messenger
  "thread_id": "120363408634625681@g.us",   // where a reply lands (chat id / group JID)
  "reply_to": "",                           // id this message answers ("" = not a reply)
  "ts": 1783138037950,                      // unix millis
  "meta": {},                               // optional producer annotations, never secrets
  "attachments": [{                         // optional media riding the message
    "type": "document",                     //   image | video | audio | voice | document | file
    "name": "report.pdf",
    "mime": "application/pdf",
    "path": "…/media/report.pdf",           //   local file under $MESSENGER_HOME/media
    "url": "",                              //   remote reference (webhook inbound / outbound fetch)
    "size": 12345
  }]
}
```

`attachments` is omitted when empty. Inbound media is downloaded into
`$MESSENGER_HOME/media` and referenced by `path`; a remote consumer fetches the bytes
via `GET /media/<basename of path>` (below).

## Auth

When `serveTokenEnv` is set in `config.toml` (default name `MESSENGER_SERVE_TOKEN`),
`POST /send` and `GET /inbox` require:

```
Authorization: Bearer <value of that env var>
```

Constant-time compared; 401 otherwise. `/health` and the inbound channel webhooks are
never bearer-auth'd (webhooks carry their own per-channel HMAC/secret).

## GET /health

```json
{"ok": true, "service": "messenger",
 "channels": {"ops": "whatsapp", "mybot": "telegram"},
 "streams": {"whatsapp": {"running": true, "restarts": 0, "started_at": "2026-07-13T10:00:00Z"}}}
```

`"service":"messenger"` is the single-instance probe: clients (and `serve` itself) use
it to detect a running hub and REUSE it rather than start another.

`streams` reports per-kind liveness of supervised STREAMING channels (whatsapp's single
`wacli` subprocess): `running`, cumulative `restarts`, `started_at`, and — if it has failed
— `last_exit_at` and `last_error`. A listener that died and is looping in backoff reads
`running:false` / rising `restarts` / a `last_error`, so a monitor catches it WITHOUT
`pgrep`/`whoami` forensics. The map is empty when no streaming channels are configured.

## POST /send

Request:

```json
{"channel": "ops", "text": "on it", "to": "<thread, optional>", "reply_to": "<id | \"last\", optional>",
 "file": "<local path or http(s) URL, optional>"}
```

- `text` OR an attachment is required (`400` when both are missing). With an
  attachment, `text` becomes the caption.
- `file` is shorthand for ONE attachment: a local file path or an `http(s)` URL. For
  full control send `"attachments": [{type, name, mime, path, url, size}]` instead.
  A `url` attachment is fetched by the platform (telegram is handed the URL directly;
  whatsapp downloads then uploads).
- `to` omitted → the channel's configured default target (`--chat-id` / `--group`).
- `reply_to: "<id>"` → threads onto that message (telegram `reply_to_message_id`,
  wacli `--reply-to`, webhook echo).
- `reply_to: "last"` → resolves to the NEWEST inbound envelope on the channel (scoped
  to `to`'s thread when given) and inherits its thread. `409` if the conversation is
  empty.

Response `200`:

```json
{"ok": true, "id": "421"}
```

`id` is the PROVIDER-assigned message id of your outbound message (telegram
`message_id`, wacli id; falls back to the minted envelope id) — pass it as a future
`reply_to` to thread onto your own send. Errors: `400` bad/missing fields, `401` auth,
`409` reply_to:"last" with no history, `422` the channel is inbound-only (see below),
`502` a genuine downstream delivery failure (body = reason).

**`422` inbound-only vs `502` gateway failure.** A webhook channel with no
`options.callbackURL` can RECEIVE (signed `POST /webhook/<name>`) but has no outbound
target, so `/send` to it can never succeed. That is a config precondition, not a transient
fault, so it answers a LOUD, structured `422` — not a `502`, which reads as an upstream
blip and invites endless retries:

```json
{"ok": false, "error": "channel: webhook \"hook\": channel: inbound-only, no outbound target configured (set options.callbackURL to enable /send)", "channel": "hook", "reason": "inbound_only"}
```

Set `options.callbackURL` to make the channel outbound-capable. A `502` is reserved for a
configured downstream that is unreachable or returns a non-2xx. (This resolves the historic
"`/send` 502s silently while the signed webhook path works" asymmetry: outbound needs a
delivery target; inbound does not.)

## GET /inbox?since=N

Poll surface (ad-hoc consumers; services should prefer subscriptions).

```json
{"messages": [<Envelope>, …], "next": 7}
```

`since` is a 1-based line offset; `0` = from the start. Pass the returned `next` back
as the following call's `since`; unchanged `next` = nothing new. Offsets index the
append-only inbox file, so they are stable across restarts.

## GET /media/\<file>

Serves one file from `$MESSENGER_HOME/media` — the store every inbound attachment's
`path` points into. Bearer-auth'd exactly like `/inbox`. `<file>` is a basename only
(path traversal is rejected); `404` when absent. Typical flow: read an envelope, take
`basename(attachment.path)`, GET it:

```sh
curl -sS http://127.0.0.1:14310/media/report.pdf \
  -H "Authorization: Bearer $MESSENGER_SERVE_TOKEN" -o report.pdf
```

## Inbound channel webhooks (same port)

- `POST /telegram/<name>` — the Telegram Bot API webhook (register via
  `messenger channel connect <name> --public-url …`). Optionally verified against
  Telegram's `secret_token` header (`--option secretHeader=…`).
- `POST /webhook/<name>` (legacy alias `/hook/<name>`) — generic signed inject:
  header `X-Hub-Signature-256: sha256=<hex HMAC-SHA256 of the raw body>` with the
  channel's secret. Body fields (all optional): `text|message`, `sender|login`,
  `thread_id`, `reply_to`, `id`. Non-JSON body → becomes `text`. Replies `202`,
  `401` bad signature. Full recipe: `../skills/messenger/references/webhook.md`.

## Subscription pushes (hub → consumer)

Registered via `messenger subscribe add <name> --url URL [--channels a,b] [--secret-env NAME]`.

- The hub POSTs each Envelope (JSON body) to the URL **in order**; your endpoint must
  return 2xx.
- The subscription's cursor advances only on 2xx — fail or be down and the SAME message
  retries with backoff (1s→60s), then everything newer follows: at-least-once, in-order
  catch-up. **Dedupe by envelope `id`.**
- With `--secret-env`, each push carries `X-Messenger-Signature-256: sha256=<hex>` —
  verify exactly like a GitHub webhook (HMAC-SHA256 over the raw body).
- Pushed envelopes include `attachments`; the media bytes stay on the hub — fetch them
  via `GET /media/<basename of path>` (bearer-auth'd).
- To answer a pushed message: `POST /send {"channel": env.channel, "text": …,
  "reply_to": env.id}` (or `"last"`).

## Errors, limits, encoding

- Request bodies are capped at 1 MiB. All JSON is UTF-8.
- Non-2xx bodies are plain-text reasons, safe to log (never contain secret values).
- The hub is single-instance per host; if `POST /send` gets connection-refused, check
  `GET /health` and start `messenger serve` (it self-guards against double-start).
