# teams channels — one Azure Bot Framework bot per channel

Read this when creating/adding/connecting/debugging a **Microsoft Teams** channel.
The `teams` kind is a real **Teams bot** (Azure Bot Framework app) — it SENDS and
RECEIVES text **and attachments** under its own bot identity. It is NOT the
incoming-webhook connector; the connector can't do inbound or attachments.

## Model — exactly like whatsapp (one bot, channels = conversations)

- The host has **ONE bot** (App ID by NAME `--user-env TEAMS_BOT_APP_ID` + client secret
  by NAME `--token-env TEAMS_BOT_PASSWORD` + `--option tenantId=`). Every `teams` channel
  is a named **conversation** on that one bot — just like whatsapp channels are groups on
  one paired device. All teams channels share the SAME credentials (the hub enforces it).
- **ONE shared inbound webhook** `/webhook/teams` serves every teams channel, no matter
  how many. The hub verifies the Bot Connector JWT (RS256, issuer
  `https://api.botframework.com`, audience = the App ID) once, then **routes each Activity
  by `conversation.id`** to the channel bound to it (`--conversation <id>`).
- A **conversation-less** teams channel is the **CATCH-ALL**: it receives every
  conversation no other teams channel binds — that's how you discover conversation ids.
  With no catch-all, an unbound conversation is dropped (same as whatsapp dropping an
  unbound group). At most one catch-all is allowed.
- `id` = the Activity id, `thread_id` = the Teams conversation id, `sender` = the Teams
  user. `serviceUrl` is learned per-conversation from inbound and recorded so a proactive
  `send` reaches it.

**No Graph API anywhere** — the bot itself sends and receives everything (this is the
whole reason to use a bot instead of the connector/Graph delta polling).

## Create the bot (owner, in Azure — condensed; full runbook: `docs/TEAMS-BOT-SETUP.md`)

Replace `<public>` with the hub's public HTTPS base (e.g. `https://msg.deemwar.com`).

1. **Azure Portal → Create a resource → "Azure Bot" → Create.** App type
   **Multi-tenant** (simplest; **Single-tenant** → note the Tenant ID for
   `--option tenantId=`). Creation type **Create new Microsoft App ID**.
2. **App ID + secret.** Bot → **Configuration** → copy **Microsoft App ID**
   (→ `TEAMS_BOT_APP_ID`). **Manage Password** → **Certificates & secrets** →
   **New client secret** → copy the **Value** once (→ `TEAMS_BOT_PASSWORD`).
   Export both where the hub runs — by NAME, never commit the value:
   ```sh
   export TEAMS_BOT_APP_ID='YOUR_APP_ID_HERE'
   export TEAMS_BOT_PASSWORD='YOUR_CLIENT_SECRET_HERE'
   ```
3. **Messaging endpoint.** Bot → **Configuration** → set it to `https://<public>/webhook/teams`.
4. **Enable Teams.** Bot → **Channels** → **Microsoft Teams** → agree + Apply.
5. **Sideload the app package.** Fill the `FILL:` fields in `teams-app/manifest.json`
   (`id` = a NEW GUID, `bots[0].botId` = the App ID, `validDomains` = `<public>` host),
   add `color.png` (192×192) + `outline.png` (32×32), zip the three flat, then
   Teams → **Apps → Manage your apps → Upload a custom app** → add to the target channel.
6. **Get the conversation id.** @mention the bot once in the target channel; the hub
   logs the inbound envelope — read its `conversation.id` (via `messenger listen` or the
   inbox) and set it as `--conversation`.

## Wire it into messenger

Add the **catch-all** first (no `--conversation`) so you can see conversation ids as the
bot is added to Teams; then add **named channels** bound to specific conversations.

```sh
# 1. catch-all — receives every conversation the bot is in that nothing else binds
messenger channel add teams teams \
  --token-env TEAMS_BOT_PASSWORD --user-env TEAMS_BOT_APP_ID --option tenantId=<tenant>

messenger channel connect teams --public-url https://<public>   # prints the endpoint (step 3)
messenger channel test teams                                    # probes creds, no send

# 2. @mention the bot in a Teams channel; read the inbound envelope's thread_id
messenger listen        # or: curl the /inbox — thread_id is the conversation id

# 3. bind a named channel to that conversation (same bot creds — enforced)
messenger channel add teams eng \
  --token-env TEAMS_BOT_PASSWORD --user-env TEAMS_BOT_APP_ID --option tenantId=<tenant> \
  --conversation <conversation-id>
```

Now inbound from that conversation arrives as `channel=eng`; everything else falls to the
`teams` catch-all. Reply with `--channel eng` (targets its bound conversation) — no Graph,
no manual serviceUrl. All teams channels MUST share the same `--token-env`/`--user-env`/
`tenantId` (one bot per host); the hub rejects a mismatch and duplicate conversation binds.

## Verify

```sh
messenger channel test teams
# ✓ teams "teams": AAD client-credentials token OK   ← acquires an AAD token, sends nothing
```

Failure modes: `env TEAMS_BOT_APP_ID is unset` / `env TEAMS_BOT_PASSWORD is unset` →
export them where the hub runs; `AAD token FAILED: status 401` → App ID/secret mismatch
or the secret expired (mint a new one in Certificates & secrets).

## Send / reply

```sh
messenger send --channel teams --text "deploy done"                       # → the --conversation target
messenger send --channel teams --text "here" --to <conversation-id>       # another conversation
messenger send --channel teams --text "ack" --reply-to <activity-id>      # threads the reply
messenger send --channel teams --file chart.png --text "caption"          # attachment (text optional)
```

`/send` returns the Bot Connector Activity id — usable as a future `reply_to`.

## Options (`--option k=v`)

| option | meaning |
|--------|---------|
| `conversationId` | default target (also `--conversation`) |
| `serviceUrl` | initial/fallback Bot Connector base; auto-updated from each inbound |
| `tenantId` | set for a **single-tenant** bot (switches the AAD token endpoint) |
| `channelId` | optional Teams channel id, informational |
| `path` | override the ONE shared inbound mount (default `/webhook/teams`; taken from the first channel) — rarely needed |
| `publicURL` | hub public base; lets outbound attach local media as a `/media/<file>` link |
| `insecureSkipJWT` | `"true"` disables inbound JWT verification — **local/dev behind a trusted proxy ONLY** |

## Gotchas

- Attachments are the reason to use `teams` over the connector — the connector can't do them.
- No public URL yet? Inbound waits until the messaging endpoint is reachable; outbound
  (`send`) works regardless once creds resolve and a `conversationId`/`serviceUrl` exists.
- The client secret **expires** (Azure default 6 months) — `channel test teams` failing
  with 401 is usually that. Mint a new secret, re-export `TEAMS_BOT_PASSWORD`, restart the hub.
