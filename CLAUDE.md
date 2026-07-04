# CLAUDE.md

Guidance for Claude Code / Codex / opencode working in **messenger**.

## What this is

`messenger` is a standalone, **broker-free conversation hub** — one static Go binary
(`CGO_ENABLED=0`) that owns receiving and sending messages over **telegram**, **whatsapp**
(shells `wacli`), and generic **webhook** channels, and delivers inbound to N consumers
via durable per-consumer subscriptions. Anything consumes it over plain HTTP; no broker.
The **CLI is the whole interface** — there is no Taskfile. Design: `docs/SPEC.md`.

Every message is a self-describing `envelope.Envelope`; replies **thread** via `reply_to`,
and `send` returns the provider message id. Kinds have different multiplicity: telegram =
many channels each its own bot; **whatsapp = ONE global paired device, each channel = a
GROUP** (one shared wacli stream, routed by group JID, `--group`-less channel = catch-all);
webhook = every hook its own channel.

## Layout

| path | what |
|------|------|
| `cmd/messenger/` | the CLI (setup/status/channel/subscribe/listen/send/serve) |
| `envelope/` | the one canonical value on the wire (id, channel, thread_id, reply_to, …) |
| `config/` | TOML config: channels + subscriptions + secret NAMES (never values) |
| `home/` | the on-disk home (`$MESSENGER_HOME` or `~/.config/messenger`) |
| `channel/` | the ONE Channel interface + KindSpec + telegram/whatsapp/webhook + runtime + age vault |
| `subscription/` | durable consumer push: per-consumer cursor + retry + HMAC-signed delivery |
| `inbox/` | append-only NDJSON inbound store + read-since (the delivery queue) |
| `server/` | the `serve` HTTP surface |
| `skills/` | the portable agent skill + `install.sh` |
| `docs/SPEC.md` | the v2 spec (conversation-hub model) |

## Rules

- **The CLI is the interface.** No Taskfile. Green gate:
  `CGO_ENABLED=0 go build ./... && go vet ./... && CGO_ENABLED=0 go test ./...`
- **Single static binary.** `CGO_ENABLED=0` must always build. Pure-Go deps only
  (`filippo.io/age`, `pelletier/go-toml/v2`). A CGO dep is a design bug.
- **Secrets are use-only.** Reference by env-var NAME (`TELEGRAM_BOT_TOKEN`,
  `MESSENGER_HOOK_SECRET`, `MESSENGER_SERVE_TOKEN`); resolve host-only at the point of use.
  Never print, log, commit, or bake a value; use `YOUR_KEY_HERE` in examples.
- **One interface per channel kind.** New kinds (teams, slack, …) implement
  `channel.Channel` (+ `Pushed` or a kind-level `Streamer`) and call
  `channel.Register(KindSpec{...})` — one file, never a parallel registry.
- **One hub per host.** `serve`/`listen` probe the addr and reuse a running instance
  (`/health` → `"service":"messenger"`); never engineer around this to double-start.
- **Conventional commits** (`feat:`, `fix:`, `docs:`, `test:`, `chore:`). Do NOT add
  `Co-authored-by:`.
- Touch only what the task needs; note unrelated issues rather than fixing them inline.

## Common commands

```sh
CGO_ENABLED=0 go build -o messenger ./cmd/messenger
CGO_ENABLED=0 go build ./... && go vet ./... && CGO_ENABLED=0 go test ./...   # green gate
messenger setup && messenger status
messenger serve
skills/install.sh        # symlink the agent skill into ~/.claude/skills
```
