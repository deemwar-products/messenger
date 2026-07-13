# Harden messenger: loud outbound errors + observable channel liveness

## Why

`messenger` is the deemwar **ears/mouth organ** — the CEO hears the owner + workers
through it and desks (crypto, stock) reach back through its channels. Three real defects
undercut that trust:

1. **`/send` 502s "silently".** POSTing `/send` to an inbound-only webhook channel (one
   with no `options.callbackURL`) returns **HTTP 502 Bad Gateway** with the body
   `channel: webhook "x": no callbackURL`. 502 reads as a transient upstream/proxy fault,
   so callers retry forever instead of fixing config — the failure is *loud in the logs of
   the wrong system*. Meanwhile the signed **inbound** path (`POST /webhook/<name>`,
   `POST /hook/send`) needs no callback and works. That asymmetry is the root cause of the
   "send 502s, signed webhook works" report: outbound needs a delivery target, inbound
   does not, and the server conflates a **config precondition** with a **gateway failure**.

2. **Channel liveness is invisible.** `/health` reports configured channels as `name→kind`
   but never whether a streaming channel (whatsapp's single `wacli` subprocess) is actually
   *running*. A listener that died and is stuck in backoff still shows up identically to a
   healthy one — you can only tell via `pgrep`/`whoami` guesswork on the box. There is no
   liveness signal a monitor can poll.

3. **macOS SIGKILLs a freshly-rebuilt binary** (ad-hoc code-signature invalidated on
   overwrite) — an ops papercut that makes "rebuild and run" fail mysteriously.

## What Changes

- **Distinguish "channel can't do outbound" from "gateway failed."** Introduce a typed
  sentinel `channel.ErrNoOutbound`; a webhook channel with no `callbackURL` returns it.
  `POST /send` maps it to **HTTP 422** with a structured, actionable JSON body
  (`{"ok":false,"error":...,"channel":...,"reason":"inbound_only"}`) instead of a bare 502.
  Genuine upstream delivery failures (callback network error / non-2xx) stay **502**.
- **Surface per-stream liveness in `/health`.** The runtime supervisor already knows each
  streaming channel's lifecycle; record it (`running`, `restarts`, `startedAt`,
  `lastErr`, `lastExitAt`) and expose a `streams` map in `/health` so a dead/looping
  whatsapp listener is observable without shell forensics. Scale-invariant: works for one
  stream or a fleet.
- **Document the codesign rebuild fix** (`xattr -c` + `codesign -s -`) as the supported
  macOS build step, and ship a tiny repo build helper so "rebuild and run" is reliable.
- **Document the DLQ / head-of-line-blocking limitation** of the current at-least-once
  subscription loop as a named follow-up requirement (a permanently-failing consumer
  blocks its own queue forever; best practice is a dead-letter after N attempts).

## Impact

- Affected specs: `messenger-hub` (send semantics, health surface).
- Affected code: `channel/webhook.go`, `channel/runtime.go`, `server/server.go`; new tests
  in `server/`, `channel/`; docs `docs/API.md`, `docs/ARCHITECTURE.md`, `README.md`.
- Backward compatible: successful sends and the inbound/hook paths are unchanged; only the
  *error status* for an inbound-only `/send` moves 502→422, and `/health` gains a field.

## Research grounding (cite-or-abstain)

- Webhook HMAC signing model (HMAC-SHA256, `sha256=` prefix, constant-time compare over the
  exact raw body, reject on failure): GitHub, *Validating webhook deliveries*
  <https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries>. messenger
  already matches this (`hmac.Equal`, `sha256=` prefix); it returns 401 (invalid credential)
  where GitHub suggests 403 — noted, not changed.
- At-least-once delivery = persistent store + ack-on-success cursor + retry-with-backoff:
  OneUptime, *How to Build At-Least-Once Delivery*
  <https://oneuptime.com/blog/post/2026-01-30-at-least-once-delivery/view>. messenger's
  NDJSON inbox + per-consumer cursor advanced only on 2xx + exponential backoff matches all three.
- Dead-letter / poison-message handling (a failing message must not block the queue
  head-of-line; move to a DLQ after N attempts): task-queues.com, *Dead-Letter Queues &
  Poison-Message Handling* <https://www.task-queues.com/queue-fundamentals-architecture/dead-letter-queues-poison-messages/>.
  Documented here as a known limitation + follow-up requirement.
