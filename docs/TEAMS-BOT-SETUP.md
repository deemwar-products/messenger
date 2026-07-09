# Microsoft Teams bot channel — owner setup

The `teams` kind is a **Teams bot** (Azure Bot Framework app), not the incoming-webhook
connector. A bot can SEND and RECEIVE text **and attachments** to/from a specific Teams
channel under its own bot identity. This is the only Teams transport that does
attachments; the connector cannot.

One `teams` channel = one bot bound to one conversation. Secrets are referenced by env
NAME only — no secret value ever lives in config, logs, or the repo.

---

## What you (the owner) must do in Azure

Replace `<public>` with the hub's public HTTPS base (e.g. `https://msg.deemwar.com`).
The messaging endpoint you register is:

```
https://<public>/webhook/teams
```

### 1. Create the Azure Bot
1. Azure Portal → **Create a resource** → search **Azure Bot** → **Create**.
2. **Bot handle**: any unique name. **Type of App**: **Multi-tenant** (simplest) or
   **Single-tenant** (then note the Tenant ID → set option `tenantId`).
3. **Creation type**: *Create new Microsoft App ID*. Create.

### 2. Copy the App ID and create a client secret
1. Open the bot → **Configuration** → copy **Microsoft App ID**.
   → this is `TEAMS_BOT_APP_ID`.
2. Click **Manage Password** (opens the App Registration) →
   **Certificates & secrets** → **New client secret** → copy the **Value** (once only).
   → this is `TEAMS_BOT_PASSWORD`.

Export both on the hub host (never commit them):
```sh
export TEAMS_BOT_APP_ID='<the App ID>'
export TEAMS_BOT_PASSWORD='<the client secret value>'
```

### 3. Set the messaging endpoint
Bot → **Configuration** → **Messaging endpoint**:
```
https://<public>/webhook/teams
```
Apply. (The hub verifies the Bot Connector JWT on every inbound POST, so the endpoint is
safe to expose.)

### 4. Enable the Teams channel
Bot → **Channels** → **Microsoft Teams** → agree + **Apply**.

### 5. Build & sideload the Teams app package
1. Take `teams-app/manifest.json` (in this repo) and fill the fields marked
   `FILL:` — at minimum:
   - `id` → a NEW GUID for the app package (NOT the bot App ID; any GUID generator).
   - `bots[0].botId` → your **App ID** (`TEAMS_BOT_APP_ID`).
   - `developer.*`, `name.*`, `description.*` → your details.
   - `validDomains` → your `<public>` host (no scheme), e.g. `msg.deemwar.com`.
2. Add two icons next to the manifest: `color.png` (192×192) and `outline.png`
   (32×32, transparent).
3. Zip the three files **flat** (manifest at the zip root):
   ```sh
   cd teams-app && zip ../teams-app.zip manifest.json color.png outline.png
   ```
4. In Teams → **Apps** → **Manage your apps** → **Upload an app** →
   **Upload a custom app** → pick `teams-app.zip` → **Add to a team** → choose the target
   team/channel.

### 6. Get the conversation id
The bot learns `serviceUrl` + `conversation.id` from the first inbound message. Send any
message that @mentions the bot in the target channel; the hub logs it and persists the
`serviceUrl` in memory. Read the `conversation.id` from the inbound envelope
(`messenger listen` / the inbox) and use it as `--conversation`.

---

## Wire it into messenger

```sh
messenger channel add teams teams \
  --conversation <conversation-id> \
  --token-env TEAMS_BOT_PASSWORD \
  --user-env  TEAMS_BOT_APP_ID

# print the endpoint to register (matches step 3):
messenger channel connect teams --public-url https://<public>

# probe credentials without sending (acquires an AAD token):
messenger channel test teams
```

### Options
| option | meaning |
|--------|---------|
| `conversationId` | target channel's conversation id (also settable via `--conversation`) |
| `serviceUrl` | initial/fallback Bot Connector base; auto-updated from each inbound activity |
| `tenantId` | set for a **single-tenant** bot (switches the AAD token endpoint) |
| `channelId` | optional Teams channel id, informational |
| `path` | override the inbound mount (default `/webhook/teams`) — set when running a 2nd teams bot |
| `publicURL` | hub public base; lets outbound attach local media as a `/media/<file>` link |
| `insecureSkipJWT` | `"true"` disables inbound JWT verification — **local/dev behind a trusted proxy only** |

---

## How it works (for maintainers)

- **Outbound Send**: OAuth2 client_credentials against
  `https://login.microsoftonline.com/botframework.com/oauth2/v2.0/token` (or the AAD
  single-tenant endpoint when `tenantId` is set), scope
  `https://api.botframework.com/.default`; token cached until ~1 min before expiry. Then
  `POST {serviceUrl}/v3/conversations/{conversationId}/activities` with a
  `{type:"message", text, attachments}` Activity. Returns the Activity id.
- **Inbound**: Bot Framework POSTs an Activity to `/webhook/teams`. The hub validates the
  `Authorization: Bearer` JWT (RS256) against the Bot Connector OpenID metadata
  (`https://login.botframework.com/v1/.well-known/openidconfiguration` → jwks): issuer
  `https://api.botframework.com`, audience = the App ID, not expired. It extracts text,
  from, `conversation.id`, `serviceUrl` (persisted for proactive sends), and downloads
  each attachment's `contentUrl` (bearer the AAD token) into the hub media store.
