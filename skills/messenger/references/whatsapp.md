# whatsapp channels — one global device, many groups

Read this when adding/connecting/debugging a whatsapp channel.

## Model

- The host has **ONE paired device** (wacli's linked WhatsApp-Web session, store in
  `~/.wacli`). It belongs to the HOST, not to any channel. Exactly one
  `wacli --json sync --follow` stream runs no matter how many whatsapp channels exist.
- Each messenger channel = a **GROUP**: `messenger channel add whatsapp ops --group
  123456789@g.us`. Inbound from that group JID arrives as `channel:"ops"`.
- A channel with **no `--group`** is the **catch-all**: DMs and unconfigured groups
  land there (with the real chat JID in `thread_id`, so you can still reply).
- `thread_id` is the group JID; `sender` is the member who wrote; `id` is wacli's
  stable message id (feed it to `--reply-to` → wacli `send … --reply-to`).

## Pairing (once per host — NEVER re-pair a linked device)

```sh
messenger channel connect <name>
# already linked → prints "already linked (<jid>)" + a table of known groups with JIDs
# not linked     → runs `wacli auth` (QR scan) once, for ALL whatsapp channels
```

To find a group JID: `messenger channel connect <any-whatsapp-channel>` lists them
(from wacli's local store — run `wacli sync` first if the table is empty).

## Verify

```sh
messenger channel test ops
# ✓ device linked: <jid> (global — serves every whatsapp channel)
# ✓ group 1234…@g.us known: <group name>     (WARNING if the JID isn't in the store)
```

Deeper: `wacli doctor --json` → `authenticated`, `linked_jid`, `connected`,
store counts. `wacli groups list` / `wacli chats list` for JIDs.

## Send / reply

```sh
messenger send --channel ops --text "hi"                    # → the channel's group
messenger send --channel ops --text "hi" --to <other-jid>   # any chat on the device
messenger send --channel ops --text "on it" --reply-to last # quote the newest inbound
messenger send --channel ops --file report.pdf --text "caption"   # media (--text optional)
```

## Attachments

- **Inbound** media is auto-downloaded (`wacli media download --chat <jid> --id
  <msgid>`) into `$MESSENGER_HOME/media`; the envelope carries `attachments[].path`.
  A failed download never blocks publish — the attachment rides metadata-only.
- **Outbound** `--file` maps to `wacli send file --to <jid> --file <path> --caption
  <text> [--reply-to <id>]`. A `url` attachment is downloaded first, then uploaded
  (WhatsApp can't fetch URLs itself).
- **Voice notes**: an attachment with `"type":"voice"` adds `--ptt`, which renders an
  OGG/Opus file as a proper WhatsApp voice-note (PTT) bubble instead of a generic file
  attachment. The `--file`/`file` shorthand always types the attachment `"file"` — send
  a full `attachments:[{"type":"voice","path":"<ogg path>"}]` (via `/send` JSON or the
  Go API) to get PTT rendering. WhatsApp expects OGG container + Opus codec, 16kHz mono
  is the safe default (`ffmpeg -i in.mp3 -ar 16000 -ac 1 -c:a libopus out.ogg`).

## Gotchas

- Two whatsapp channels with the same `--group` = second one never receives (first
  match wins on routing).
- Threaded replies use wacli's `--reply-to` flag; older docs said `--quote` — that
  flag never existed in wacli.
- wacli holds a store lock: don't run long `wacli sync` manually while the hub is up.
- The stream is supervised with backoff; if wacli crashes it restarts automatically
  (`messenger serve` logs "whatsapp stream exited … restarting").
- Options: `--option bin=<path>` overrides the wacli binary; `--option args="…"`
  overrides the stream args (first whatsapp channel that sets them wins — they
  describe the ONE device).
