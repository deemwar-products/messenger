# AGENTS.md — the messenger contract for every agent, boot script, and desk

This is the operational law for ANY process that wants channel I/O on a host (CEO hub,
crypto desk, fleet workers, cron jobs, boot.sh). Follow it exactly and hubs never
fight, inbound never drops, and every agent can send + listen independently.
Human setup guide: `ONBOARDING.md`. API detail: `docs/API.md`. Agent recipes:
`skills/messenger/SKILL.md`.

## The one law

**ONE messenger hub per host. Agents NEVER run their own.** The WhatsApp device is a
host-global singleton — a second hub (any port, any $MESSENGER_HOME, any binary copy)
starts a second `wacli sync` stream, steals the device, and silently drops inbound.
This is exactly the 2026-07-05 incident. Everything below exists to make it impossible.

## What boot.sh (the host owner) must do — and ONLY boot.sh

boot.sh (or launchd/systemd — exactly one supervisor per host) owns the hub lifecycle:

```sh
MESSENGER_BIN=/path/to/the/ONE/canonical/messenger   # repo-local bin, never a global copy
HUB=http://127.0.0.1:14310

# 1. kill imposters — any messenger serve/listen NOT running the canonical binary
for pid in $(pgrep -f "messenger (serve|listen)"); do
  exe=$(ps -o comm= -p "$pid")
  [ "$exe" = "$MESSENGER_BIN" ] || kill "$pid"
done

# 2. ensure THE hub (idempotent — serve self-guards, but probe first anyway)
if ! curl -sf --max-time 2 "$HUB/health" | grep -q '"service":"messenger"'; then
  nohup "$MESSENGER_BIN" serve >>/var/tmp/messenger.log 2>&1 &
fi

# 3. NEVER: run wacli sync/auth yourself, run a second serve, copy the binary elsewhere
```

Channel and subscription **config changes** also belong to the host owner only.
Onboarding a new agent is ONE idempotent command (safe to keep in boot.sh):

```sh
# whatsapp lane (a group):
messenger register <agent> --group <jid> --url http://127.0.0.1:<port>/hook
# telegram lane (its own bot):
messenger register <agent> --kind telegram --token-env BOT_TOKEN [--chat-id ID] --url …
# webhook lane (its own signed path):
messenger register <agent> --kind webhook --token-env HOOK_SECRET --url …
# = channel <agent> of that kind + subscription <agent> filtered to it
#   + prints the agent's exact send/reply/listen contract.
# omit --url → poll mode; --channels a,b → attach to existing lanes instead of a new one.
```

One group JID = ONE channel — `register`/`channel add` refuse a JID that is already
bound (a duplicate bind would silently shadow the first). `channel connect <wa>` lists
only the FREE groups (bound ones hidden), so you never re-pick a taken JID.

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
  -d '{"channel":"cryptolocal-wa","text":"desk armed"}'          # → {"ok":true,"id":"…"}
```

**Reply** — answer the message you were given (`"reply_to": env.id`) or the newest one
(`"reply_to":"last"`); never send unthreaded chatter into a group you were answering.

**Listen — your OWN subscription, one per agent, named after you:**

```sh
# registered ONCE by the host owner:
messenger subscribe add cryptodesk --url http://127.0.0.1:9100/hook --channels cryptolocal-wa
```

- You get every envelope for YOUR channels POSTed in order; answer 2xx fast, process async.
- Down = automatic catch-up from your cursor. At-least-once → **dedupe by envelope `id`**.
- NEVER read another agent's cursor file, never share a subscription name.
- No HTTP endpoint? Poll `GET /inbox?since=N` with your own persisted `next` instead.

**Attachments:** inbound envelopes carry `attachments[]`; fetch bytes via
`GET /media/<basename of path>` (same bearer). Send files with `{"file":"<path|url>"}`.

**Your channel is your lane:** send only on channels assigned to you
(desk → `cryptolocal-wa`, CEO → `ops`). Different groups on one device is the DESIGNED
case — routing by group JID keeps conversations separate; don't cross lanes.

## Do / Don't

| | |
|---|---|
| ✅ probe `/health`, reuse the hub | ❌ `messenger serve` from an agent, tmux, or a second binary |
| ✅ send via `POST /send` (or `messenger send` — it's one-shot) | ❌ `wacli send` / `wacli sync` directly |
| ✅ own subscription, dedupe by `id` | ❌ polling with someone else's cursor / shared sub names |
| ✅ `reply_to: env.id` or `"last"` | ❌ unthreaded blasts into shared groups |
| ✅ ask the host owner for a new channel/group | ❌ `channel add` / re-pair from an agent |
| ✅ secrets by NAME (`$MESSENGER_SERVE_TOKEN`) | ❌ echoing a secret value into logs/chat |

## Incident playbook — "WhatsApp went silent"

1. `pgrep -f "messenger (serve|listen)"` → more than one? Kill everything that isn't
   the canonical binary; re-probe `/health`.
2. `pgrep -f "wacli.*sync"` → more than one stream = the smoking gun.
3. `messenger status` + `messenger channel test` on the surviving hub.
4. Resend a probe message; confirm it lands in `GET /inbox`.
