# AGENTS.md

The full operating contract for agents, desks, and boot scripts is
**[`skills/messenger/references/agents.md`](skills/messenger/references/agents.md)** —
the canonical copy, shipped to every agent that runs `messenger install --skills`. This
root file is the summary + pointer so it stays discoverable (GitHub, coding agents
working in the repo).

## The one law

**ONE messenger hub per host. Agents NEVER run their own.** The WhatsApp device is a
host-global singleton — a second hub (any port, any `$MESSENGER_HOME`, any binary copy)
starts a second `wacli sync` stream, steals the device, and silently drops inbound.

## Every agent, in four lines

1. **Discover, never start** — `curl -sf http://127.0.0.1:14310/health`; if it answers
   `"service":"messenger"`, use it over HTTP. Never `messenger serve`, never `wacli`.
2. **Send** — `POST /send {channel,text[,reply_to,file]}` (bearer by NAME when set).
   **Reply** with `reply_to: env.id` or `"last"`.
3. **Listen** — your OWN named subscription (the host owner runs `messenger register
   <you> … --url …`); dedupe by envelope `id`; or poll `GET /inbox?since=N`.
4. **Stay in your lane** — send only on channels assigned to you; ask the host owner to
   wire new ones.

## Host owner / boot.sh

Owns the hub lifecycle and ALL config. Kill any `messenger serve/listen` not running the
canonical binary; ensure one hub; onboard agents idempotently with `messenger register
<agent> [--group <jid> | --kind telegram|webhook --token-env NAME] --url …`.

Full rules, the boot.sh snippet, the Do/Don't table, and the "WhatsApp went silent"
incident playbook: **[`skills/messenger/references/agents.md`](skills/messenger/references/agents.md)**.
Human setup: [`ONBOARDING.md`](ONBOARDING.md).
