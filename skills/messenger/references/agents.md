# agents.md — the messenger operating contract (READ FIRST)

The operational law for ANY process that wants channel I/O on a host (CEO hub, crypto
desk, fleet workers, cron jobs, boot.sh). Follow it and hubs never fight, inbound never
drops, and every agent can send + listen independently. This ships with the skill — it
IS the reference agents onboard from. Recipes: `../SKILL.md`. Kind depth:
`whatsapp.md` / `telegram.md` / `webhook.md`.

## The one law

**ONE messenger hub per host. Agents NEVER run their own.** The WhatsApp device is a
host-global singleton — a second hub (any port, any `$MESSENGER_HOME`, any binary copy)
starts a second `wacli sync` stream, steals the device, and silently drops inbound.
Everything below exists to make that impossible.

## What EVERY agent must do (desk, worker, cron — no exceptions)

**Rule 0 — discover, never start:**

```sh
curl -sf --max-time 2 http://127.0.0.1:14310/health   # {"ok":true,"service":"messenger",...}
```

Hub answers → use it over HTTP. Hub absent → tell the host owner / trigger boot.sh.
An agent NEVER runs `messenger serve`, NEVER runs `wacli` directly (not even
`wacli sync` — it competes for the device), NEVER installs its own binary copy.

**Send** (auth header only when the hub has `serveTokenEnv` set — reference by NAME):

```sh
curl -sS -X POST http://127.0.0.1:14310/send \
  -H "Authorization: Bearer $MESSENGER_SERVE_TOKEN" -H "Content-Type: application/json" \
  -d '{"channel":"<your-channel>","text":"hello"}'          # → {"ok":true,"id":"…"}
```

**Reply** — answer the message you were given (`"reply_to": env.id`) or the newest one
(`"reply_to":"last"`); never send unthreaded chatter into a group you were answering.

**Listen — your OWN subscription, one per agent, named after you** (the host owner
registers it once with `messenger register <you> … --url http://127.0.0.1:<port>/hook`):

- You get every envelope for YOUR channels POSTed in order; answer 2xx fast, process async.
- Down = automatic catch-up from your cursor. At-least-once → **dedupe by envelope `id`**.
- NEVER read another agent's cursor file, never share a subscription name.
- No HTTP endpoint? Poll `GET /inbox?since=N` with your own persisted `next` instead.

**Attachments:** inbound envelopes carry `attachments[]`; fetch bytes via
`GET /media/<basename of path>` (same bearer). Send files with `{"file":"<path|url>"}`.

**Your channel is your lane:** send only on channels assigned to you. Different groups
on one device is the DESIGNED case — routing by group JID keeps conversations separate;
don't cross lanes.

## What boot.sh / the host owner does — and ONLY the host owner

One supervisor per host (boot.sh / launchd / systemd) owns the hub lifecycle AND all
config changes (`channel add/remove`, `subscribe`, `register`). Agents request; the
owner wires.

```sh
MESSENGER_BIN=/path/to/the/ONE/canonical/messenger   # repo-local bin, never a global copy
HUB=http://127.0.0.1:14310

# 1. kill imposters — any messenger serve/listen NOT running the canonical binary
for pid in $(pgrep -f "messenger (serve|listen)"); do
  [ "$(ps -o comm= -p "$pid")" = "$MESSENGER_BIN" ] || kill "$pid"
done
# 2. ensure THE hub (idempotent — serve self-guards, probe first anyway)
curl -sf --max-time 2 "$HUB/health" | grep -q '"service":"messenger"' \
  || nohup "$MESSENGER_BIN" serve >>/var/tmp/messenger.log 2>&1 &
# 3. onboard agents (idempotent — safe every boot). Any kind:
$MESSENGER_BIN register <agent> --group <jid> --url http://127.0.0.1:<port>/hook       # whatsapp group
$MESSENGER_BIN register <agent> --kind telegram --token-env BOT_TOKEN --url …          # own bot
$MESSENGER_BIN register <agent> --kind webhook  --token-env HOOK_SECRET --url …        # own signed path
# NEVER: run wacli sync/auth yourself, run a second serve, copy the binary elsewhere.
```

One group JID = ONE channel — `register`/`channel add` refuse a JID already bound;
`channel connect <wa>` lists only the FREE groups (bound ones hidden).

## Do / Don't

| | |
|---|---|
| ✅ probe `/health`, reuse the hub | ❌ `messenger serve` from an agent, tmux, or a second binary |
| ✅ send via `POST /send` (or `messenger send`) | ❌ `wacli send` / `wacli sync` directly |
| ✅ own subscription, dedupe by `id` | ❌ polling with someone else's cursor / shared sub names |
| ✅ `reply_to: env.id` or `"last"` | ❌ unthreaded blasts into shared groups |
| ✅ ask the host owner for a channel/group | ❌ `channel add` / re-pair from an agent |
| ✅ secrets by NAME (`$MESSENGER_SERVE_TOKEN`) | ❌ echoing a secret value into logs/chat |

## Incident playbook — "WhatsApp went silent"

1. `pgrep -f "messenger (serve|listen)"` → more than one? Kill everything that isn't
   the canonical binary; re-probe `/health`.
2. `pgrep -f "wacli.*sync"` → more than one stream = the smoking gun.
3. `messenger status` + `messenger channel test` on the surviving hub.
4. Resend a probe message; confirm it lands in `GET /inbox`.
