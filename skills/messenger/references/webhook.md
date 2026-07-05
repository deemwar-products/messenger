# webhook channels — every hook is its own channel

Read this when injecting messages from scripts/CI, or bridging another system in/out.

## Model

- Each webhook channel = its own inbound path **`/webhook/<name>`** (legacy
  `/hook/<name>` still answers; override: `--option path=/custom`) + its own HMAC
  secret (by NAME: `--token-env MESSENGER_HOOK_SECRET` or any var name).
- Inbound: any caller that can sign the body can inject an Envelope. Outbound
  (optional): set `--option callbackURL=https://…` and `send --channel <name>` POSTs
  the envelope there, signed the same way. No callbackURL = inbound-only.

## Setup

```sh
messenger channel add webhook incoming --token-env MESSENGER_HOOK_SECRET
messenger channel add webhook bridge --token-env BRIDGE_SECRET --option callbackURL=https://other-system/hook
messenger channel test incoming    # secret resolvable? path? callback?
```

## Inject a message (inbound)

Same host as the hub? Use the verb — it loads the channel from config, resolves the
secret by NAME the same way the server does, signs the exact raw bytes, and POSTs to
the running hub:

```sh
messenger inject --channel incoming --text "deploy done" --sender ci --thread run-42 [--reply-to MSGID] [--addr :14310]
# → injected id=… channel=incoming    (202 → envelope flows to inbox + all subscriptions)
```

Non-zero exit + a one-line hint on failure: 401 = the hub resolves a different secret
than your shell; "hub unreachable" = start it (`messenger serve`).

**Cross-host fallback** (no binary/config where the caller runs): sign the raw body
with HMAC-SHA256, hex, prefixed `sha256=`:

```sh
body='{"text":"deploy done","sender":"ci","thread_id":"run-42"}'
sig="sha256=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$MESSENGER_HOOK_SECRET" -hex | awk '{print $NF}')"
curl -sS -X POST http://<hub-host>:14310/webhook/incoming -H "X-Hub-Signature-256: $sig" -d "$body"
# 202 Accepted → envelope flows to inbox + all subscriptions
```

Accepted body fields (all optional, best-effort): `text` (or `message`), `sender` (or
`login`), `thread_id`, `reply_to`, `id` (your own stable id; else one is minted),
`attachments` (`[{type, name, mime, url, size}]` — remote references passed through;
an inbound `path` is IGNORED, a caller must not name the hub's local files).
A non-JSON body becomes the `text` verbatim. Header override:
`--option signatureHeader=X-Custom-Sig`.

## Outbound (callback)

`messenger send --channel bridge --text "hi" [--file F] [--to T] [--reply-to ID]` POSTs
the full Envelope JSON — `attachments` included — to `callbackURL` with
`X-Hub-Signature-256` (when a secret is set). The receiver verifies the same way GitHub
webhooks are verified. `reply_to`/`thread_id` are echoed — the receiver decides what
threading (and media fetching) means.

## Gotchas

- 401 "signature verification failed" → body was mutated in transit (whitespace,
  re-encoding) — sign the EXACT bytes you send; don't pipe through tools that pretty-print.
- 500 "unconfigured" → the secret env var isn't set where the hub runs.
- The signature covers the raw body only; use HTTPS if the path crosses hosts.
