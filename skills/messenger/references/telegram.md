# telegram channels — one bot per channel

Read this when adding/connecting/debugging a telegram channel.

## Model

- Each telegram channel is its **OWN bot**: own token (by NAME: `--token-env
  TELEGRAM_BOT_TOKEN` or any var name you choose), own default target (`--chat-id`).
  Many telegram channels coexist (different bots).
- **One bot has ONE webhook** — Telegram delivers each bot's updates to a single URL.
  So a bot belongs to exactly one consumer; give messenger its own bot (via @BotFather)
  rather than sharing one that another system owns. Bridge an existing owner in via a
  webhook channel instead.
- Inbound arrives on `/telegram/<name>` (override: `--option path=/custom`).
  `id` = telegram `message_id`, `thread_id` = chat id, `sender` = username or user id.

## Setup

```sh
messenger channel add telegram mybot --token-env TELEGRAM_BOT_TOKEN --chat-id -1001234567890
messenger channel connect mybot --public-url https://your-host
# prints (token by NAME, never a value):
#   curl -sS "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/setWebhook" -d "url=https://your-host/telegram/mybot"
```

Run that curl yourself — the hub must be reachable at the public URL (tunnel/reverse
proxy). Optional hardening: set Telegram's `secret_token` on setWebhook and add
`--option secretHeader=X-Telegram-Bot-Api-Secret-Token` (verified against the token).

## Verify

```sh
messenger channel test mybot
# ✓ token OK — bot @yourbot (id 123456)     ← calls getMe with the token by NAME
# ✓ default target chat: -1001234567890
```

Failure modes: "env TELEGRAM_BOT_TOKEN is unset" → export it where the hub runs;
"getMe status 401" → token invalid/revoked (re-issue via @BotFather).

## Send / reply

```sh
messenger send --channel mybot --text "hi"               # → the configured --chat-id
messenger send --channel mybot --text "hi" --to 987654   # another chat (bot must be a member)
messenger send --channel mybot --text "yes" --reply-to last   # threads via reply_to_message_id
messenger send --channel mybot --file chart.png --text "caption"   # media (--text optional)
```

`/send` returns the telegram `message_id` of YOUR message — usable as a future
`reply_to` (media sends included).

## Attachments

- **Inbound** photo/document/video/voice/audio is downloaded via `getFile` →
  `/file/bot$TELEGRAM_BOT_TOKEN/<file_path>` (token by NAME, never a value) into
  `$MESSENGER_HOME/media`; the telegram caption becomes the envelope `text`.
- **Outbound** picks the Bot API method by attachment type — sendPhoto / sendVideo /
  sendVoice / sendAudio / sendDocument (`document` and `file` both send as document);
  `text` rides as the caption.
- A `url` attachment is passed to Telegram as a plain URL string — Telegram fetches
  it itself, no local download.

## Gotchas

- Group/channel chat ids are negative (`-100…`); the bot must be added to the chat
  (and for channels, as an admin that can post).
- No public URL yet? The webhook can't deliver — inbound waits until setWebhook points
  at a reachable host; outbound (`send`) works regardless.
- `--option baseURL=` exists for testing against a fake Bot API.
