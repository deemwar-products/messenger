# Telegram channel — going live

How to wire a live Telegram bot into the `messenger` hub: **send and receive text AND
attachments** (documents, photos, voice, audio, video) to/from one chat or group, over
its own bot token.

Every telegram channel is its **own bot**: one bot → one token → one webhook → one
consumer. Run this once per bot you want to bridge.

> **Secrets:** the bot token is referenced by env-var **NAME** only
> (`TELEGRAM_BOT_TOKEN`), never a value — not in config, not in git, not in any command
> output. `messenger` resolves it host-only at the point of the Bot API call.

## Is a bot even viable? (yes)

Historically the **polling** listener (`getUpdates` long-poll) was blocked from India,
so inbound looked dead. This channel does **not** poll — Telegram **POSTs** updates to
the hub's public webhook (`setWebhook`), which works fine. The crypto desk already runs a
working Telegram bot in production today, so a bot IS viable. You just need the hub
reachable at a public HTTPS URL for the webhook (a Cloudflare tunnel / any public host).

## 1. Create the bot → get the token

1. In Telegram, open [@BotFather](https://t.me/BotFather).
2. Send `/newbot`, pick a display name and a unique `@username` ending in `bot`.
3. BotFather replies with the **HTTP API token** (`123456789:AA...`). Treat it like a
   password.
4. Store it as an environment variable — **never in git**. In the shell/service that runs
   `messenger serve`:

   ```sh
   export TELEGRAM_BOT_TOKEN='PASTE_TOKEN_HERE'   # e.g. in ~/.zshrc or the service env
   ```

   Verify it loaded WITHOUT printing the value:

   ```sh
   [ -n "$TELEGRAM_BOT_TOKEN" ] && echo "token is set" || echo "NOT set"
   ```

## 2. Add the bot to the target chat / group

- **Group:** add the bot as a member. To let it read every message (not just commands),
  either make it a group **admin**, or message BotFather `/setprivacy` → your bot →
  **Disable** (group privacy off). Otherwise it only receives messages that @-mention it
  or reply to it.
- **1:1 chat:** just open a DM with the bot and send it any message once, so the chat
  exists.

## 3. Find the chat id

Send one message in the target chat, then read the update off the Bot API (the token is
consumed only inside the `curl`, never echoed):

```sh
curl -sS "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/getUpdates" \
  | python3 -c 'import sys,json; [print(u["message"]["chat"]["id"], u["message"]["chat"].get("title") or u["message"]["chat"].get("username")) for u in json.load(sys.stdin)["result"] if "message" in u]'
```

- A **group** chat id is negative (e.g. `-1001234567890` for supergroups).
- A **1:1** chat id is your positive user id.

> If `getUpdates` returns nothing, the webhook may already be set (they're mutually
> exclusive). Clear it temporarily with
> `curl -sS "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/deleteWebhook"`, read the
> id, then set the webhook back in step 5.

## 4. Add the channel to messenger

```sh
messenger channel add telegram mybot \
  --token-env TELEGRAM_BOT_TOKEN \
  --chat-id -1001234567890
```

- `mybot` is the channel name (also the `Envelope.Channel`).
- `--token-env` names the env var; the value is never read into config.
- `--chat-id` is the **default** send target. Any send can still override it with `--to`.

Confirm the token resolves and the bot is reachable (prints `@username`, never the token):

```sh
messenger channel test mybot
```

## 5. Register the webhook (inbound)

Inbound arrives when Telegram POSTs updates to the hub. The hub must be reachable at a
public HTTPS URL. Print the exact `setWebhook` call:

```sh
messenger channel connect mybot --public-url https://<your-public-host>
```

That prints (token kept out of the output — you run it against your env var):

```sh
curl -sS "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/setWebhook" \
  -d "url=https://<your-public-host>/telegram/mybot"
```

Run it. The default inbound path is `/telegram/mybot` (override with
`--option path=/custom`).

**Optional — verify the webhook caller.** Telegram can sign each POST with a secret
token header. Set it on `setWebhook` (`-d "secret_token=..."`) and tell the channel the
header name to check:

```sh
messenger channel add telegram mybot ... --option secretHeader=X-Telegram-Bot-Api-Secret-Token
```

The channel then rejects any POST whose header ≠ the resolved token.

## 6. Run the hub, then send + receive

```sh
messenger serve            # mounts /telegram/mybot and the /send surface
```

Send text:

```sh
messenger send --channel mybot --text "hello from messenger"
```

Send an attachment (local file uploaded multipart, or a URL Telegram fetches
server-side). The type is inferred → `sendDocument`/`sendPhoto`/`sendVideo`/etc.:

```sh
messenger send --channel mybot --text "the report" --file ./report.pdf
messenger send --channel mybot --to -1009999 --reply-to 55 --file https://example.com/x.png
```

**Receive:** any message in the chat (text or media) is POSTed to the hub, normalized to
an `Envelope`, and delivered to subscribers. Media (`document`/`photo`/`voice`/`audio`/
`video`) is downloaded via `getFile` into the hub media store and exposed as
`attachments[].path` — the same store the WhatsApp channel uses. Captions become the
envelope text; the sender's `message_id` becomes the envelope id so a reply threads back.
If a download fails, the envelope still ships with the metadata-only attachment — a
message is never dropped.

## Recap — the two owner-only inputs

1. **BotFather token** → exported as `TELEGRAM_BOT_TOKEN` (never committed).
2. **chat id** of the target group/DM → passed to `--chat-id`.

Everything else is `messenger channel add` / `connect` / `serve`.
