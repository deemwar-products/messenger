# agents.md — how a consumer uses messenger (CEO, crypto desk, any agent)

The operating contract for ANY process that wants channel I/O on a host. Follow it and
hubs never fight, inbound never drops, every agent sends + listens independently. This
ships with the skill — it IS the reference agents onboard from. Recipes: `../SKILL.md`.
Kind depth: `whatsapp.md` / `telegram.md` / `webhook.md`. Host setup: `../../../ONBOARDING.md`.

## The one law

**ONE messenger hub per host. Agents NEVER run their own hub or wacli.** The WhatsApp
device is a host-global singleton — a second `messenger serve` / `wacli sync` steals it
and silently drops inbound. The host owner runs the hub (as a managed service); you talk
to it over HTTP. That's the whole discipline.

## Rule 0 — discover, never start

```sh
curl -sf --max-time 2 http://127.0.0.1:14310/health    # {"ok":true,"service":"messenger","channels":{…}}
```

- Answers `"service":"messenger"` → use it over HTTP for everything below.
- Doesn't answer → tell the host owner (they run `messenger install --service`). Do NOT
  start your own hub, do NOT run `wacli`, do NOT install a second binary.

**Auth:** when the hub sets a serve token, add `-H "Authorization: Bearer
$MESSENGER_SERVE_TOKEN"` to every `/send`, `/inbox`, `/media` call. Reference the env var
by NAME; never echo its value.

## Send

```sh
curl -sS -X POST http://127.0.0.1:14310/send \
  -H "Authorization: Bearer $MESSENGER_SERVE_TOKEN" -H "Content-Type: application/json" \
  -d '{"channel":"<your-channel>","text":"hello"}'      # → {"ok":true,"id":"<provider msg id>"}
```

`to` is optional (defaults to the channel's target). Attachments: add `"file":"<local
path or http(s) URL>"`.

## Reply (threaded)

Answer the message you were handed with its id, or the newest one on the channel:

```sh
# → {"channel":"ops","text":"on it","reply_to":"<envelope id>"}     # this exact message
# → {"channel":"ops","text":"on it","reply_to":"last"}              # the newest inbound (no bookkeeping)
```

Never fire unthreaded chatter into a group you were answering.

## Receive — your OWN subscription (preferred) or poll

The host owner registers you once (`messenger register <you> --group <jid> --url
http://127.0.0.1:<port>/hook`). Then:

- Every envelope for YOUR channels is POSTed to your URL **in order**. Answer 2xx fast,
  process async.
- Down or slow? Your cursor doesn't advance past an un-acked message — you **catch up**
  from where you left off. At-least-once → **dedupe by envelope `id`**.
- No HTTP endpoint? Poll `GET /inbox?since=N` and persist the returned `next` as your `since`.
- Media: the envelope carries `attachments[]`; fetch bytes via `GET /media/<basename of
  attachments[].path>` (same bearer).

## The envelope you receive

```json
{"id":"WA123","channel":"cryptodesk","sender":"…@lid","text":"go flat",
 "thread_id":"120363410186820001@g.us","reply_to":"","ts":1783267058407,"attachments":[]}
```

`channel` is your lane's NAME. `id` + `thread_id` are all you need to reply. To answer:
`POST /send {"channel": env.channel, "text": "…", "reply_to": env.id}`.

## Worked examples

**Crypto desk** (a WhatsApp group lane, receives on its own port):
```sh
# host owner, once:  messenger register cryptodesk --group 120363410186820001@g.us --url http://127.0.0.1:9100/hook
# desk receives a POST at :9100/hook → dedupe by id → decide → reply:
curl -sS -X POST http://127.0.0.1:14310/send -H "Authorization: Bearer $MESSENGER_SERVE_TOKEN" \
  -d '{"channel":"cryptodesk","text":"flattened BTC, flat now","reply_to":"last"}'
```

**CEO** (a Telegram bot lane):
```sh
# host owner, once:  messenger register ceo --kind telegram --token-env CEO_BOT_TOKEN --url http://127.0.0.1:9000/hook
# same contract — CEO sends on channel "ceo", replies with reply_to.
```

## Do / Don't

| | |
|---|---|
| ✅ probe `/health`, use the running hub | ❌ `messenger serve` / `wacli` from an agent |
| ✅ send via `POST /send` | ❌ `wacli send` / `wacli sync` directly |
| ✅ own subscription, dedupe by `id` | ❌ another agent's cursor / a shared subscription name |
| ✅ `reply_to: env.id` or `"last"` | ❌ unthreaded blasts into shared groups |
| ✅ send only on YOUR channel(s) | ❌ crossing into another agent's lane |
| ✅ ask the host owner for a new channel | ❌ `channel add` / re-pair from an agent |
| ✅ secrets by NAME (`$MESSENGER_SERVE_TOKEN`) | ❌ echoing a secret value into logs/chat |

## Incident playbook — "WhatsApp went silent"

1. `pgrep -f "messenger (serve|listen)"` → more than one, or one that isn't the managed
   service? Kill the extras.
2. `pgrep -f "wacli.*sync"` → more than one stream = the smoking gun (two hubs).
3. On the surviving hub: `messenger status` + `messenger channel test`.
4. Send a probe; confirm it lands in `GET /inbox`.
