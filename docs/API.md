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
  "meta": {}                                // optional producer annotations, never secrets
}
```

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
{"ok": true, "service": "messenger", "channels": {"ops": "whatsapp", "mybot": "telegram"}}
```

`"service":"messenger"` is the single-instance probe: clients (and `serve` itself) use
it to detect a running hub and REUSE it rather than start another.

## POST /send

Request:

```json
{"channel": "ops", "text": "on it", "to": "<thread, optional>", "reply_to": "<id | \"last\", optional>"}
```

- `to` omitted → the channel's configured default target (`--chat-id` / `--group`).
- `reply_to: "<id>"` → threads onto that message (telegram `reply_to_message_id`,
  whatsapp `--quote`, webhook echo).
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
`409` reply_to:"last" with no history, `502` delivery failure (body = reason).

## GET /inbox?since=N

Poll surface (ad-hoc consumers; services should prefer subscriptions).

```json
{"messages": [<Envelope>, …], "next": 7}
```

`since` is a 1-based line offset; `0` = from the start. Pass the returned `next` back
as the following call's `since`; unchanged `next` = nothing new. Offsets index the
append-only inbox file, so they are stable across restarts.

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
- To answer a pushed message: `POST /send {"channel": env.channel, "text": …,
  "reply_to": env.id}` (or `"last"`).

## Errors, limits, encoding

- Request bodies are capped at 1 MiB. All JSON is UTF-8.
- Non-2xx bodies are plain-text reasons, safe to log (never contain secret values).
- The hub is single-instance per host; if `POST /send` gets connection-refused, check
  `GET /health` and start `messenger serve` (it self-guards against double-start).
