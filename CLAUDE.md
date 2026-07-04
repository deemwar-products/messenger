# CLAUDE.md

Guidance for Claude Code / Codex / opencode working in **messenger**.

## What this is

`messenger` is a standalone, **broker-free channel I/O product** — one static Go binary
(`CGO_ENABLED=0`) that owns receiving and sending messages over **telegram**, **whatsapp**
(shells `wacli`), and a generic **HMAC hook**. Anything consumes it over plain HTTP; no
broker. Extracted from the-factory's proven transport layer and made NATS-free.

Three verbs: `listen` (ingress → inbox + optional webhook push), `send` (one-shot egress),
`serve` (HTTP: `POST /send`, `GET /inbox?since=N`, `GET /health`, + channel webhooks).
Every message is a self-describing `envelope.Envelope`; replies **thread** via `reply_to`.

## Layout

| path | what |
|------|------|
| `cmd/messenger/` | the CLI (setup/listen/send/serve) |
| `envelope/` | the one canonical value on the wire (id, channel, thread_id, reply_to, …) |
| `config/` | TOML config: enabled channels + secret NAMES (never values) |
| `home/` | the on-disk home (`$MESSENGER_HOME` or `~/.config/messenger`) |
| `transport/` | registry + listener + adapters (telegram/whatsapp/hook) + age vault |
| `inbox/` | append-only NDJSON inbound store + read-since |
| `server/` | the `serve` HTTP surface |
| `skills/` | the portable agent skill + `install.sh` |

## Rules

- **Task-driven, always.** Run through `task` verbs; env loads from `.env` via the
  Taskfile `dotenv`, never the ambient shell. `task check` = build + vet + test (green gate).
- **Single static binary.** `CGO_ENABLED=0` must always build. Pure-Go deps only
  (`filippo.io/age`, `pelletier/go-toml/v2`). A CGO dep is a design bug.
- **Secrets are use-only.** Reference by env-var NAME (`TELEGRAM_BOT_TOKEN`,
  `MESSENGER_HOOK_SECRET`, `MESSENGER_SERVE_TOKEN`); resolve host-only at the point of use.
  Never print, log, commit, or bake a value; use `YOUR_KEY_HERE` in examples.
- **Conventional commits** (`feat:`, `fix:`, `docs:`, `test:`, `chore:`). Do NOT add
  `Co-authored-by:`.
- Touch only what the task needs; note unrelated issues rather than fixing them inline.

## Common commands

```sh
task check            # build + vet + test — the green gate (runs CGO_ENABLED=0)
task setup            # scaffold config.toml + home
task serve            # run the HTTP server
task install-skill    # symlink the agent skill into ~/.claude/skills
```
