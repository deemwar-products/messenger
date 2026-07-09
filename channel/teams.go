package channel

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
	"github.com/deemwar-products/messenger/home"
)

// teamsKind is the whole Microsoft Teams **bot** kind: wire behavior + CLI behavior.
// Like telegram it is a bot — an Azure Bot Framework app identified by an App (client)
// ID + client secret — that RECEIVES inbound over an HTTP webhook (Bot Framework
// Activity JSON POSTed to /webhook/teams) and SENDS outbound over the Bot Connector
// REST API. It is NOT the incoming-webhook connector (that cannot do attachments).
//
// One teams channel = one bot bound to one Teams conversation (a channel/chat). Because
// a bot has ONE messaging endpoint, a second teams channel overrides options["path"].
// serviceUrl is per-activity and per-tenant; we persist the last-seen one in memory so
// a proactive Send works between inbound messages.
type teamsKind struct{ Base }

func init() { Register(teamsKind{}) }

func (teamsKind) Name() string   { return "teams" }
func (teamsKind) Traits() Traits { return Traits{RequiresToken: true, TargetFlag: "conversation"} }

func (teamsKind) Open(name string, cfg config.Transport, res *SecretResolver) (Channel, error) {
	return openTeams(name, cfg, res)
}

func (teamsKind) Test(ctx context.Context, name string, cfg config.Transport, res *SecretResolver) ([]string, error) {
	// Probe WITHOUT sending: acquire an AAD token (proves App ID + secret resolve and
	// the Bot Connector accepts our credentials). No secret value appears in the lines.
	ch, err := openTeams(name, cfg, res)
	if err != nil {
		return nil, err
	}
	tc := ch.(*teamsChannel)
	if _, err := tc.aadToken(ctx); err != nil {
		return []string{fmt.Sprintf("teams %q: AAD token FAILED: %v", name, err)}, nil
	}
	return []string{fmt.Sprintf("teams %q: AAD client-credentials token OK", name)}, nil
}

func (teamsKind) AddHints(name string, cfg config.Transport) []string {
	return []string{
		fmt.Sprintf("set your Azure Bot messaging endpoint to https://<host>%s", teamsPath(name, cfg)),
		fmt.Sprintf("enable the Teams channel on the bot, then sideload the app package — see docs/TEAMS-BOT-SETUP.md"),
		fmt.Sprintf("App ID env: $%s  ·  client-secret env: $%s",
			envNameOr(cfg.UserEnv, "TEAMS_BOT_APP_ID"), envNameOr(cfg.TokenEnv, "TEAMS_BOT_PASSWORD")),
	}
}

// Connect prints the messaging-endpoint the owner must register in the Azure Bot — no
// secret enters this output; the App ID / secret stay NAMES.
func (teamsKind) Connect(name string, cfg config.Transport, p ConnectParams) error {
	path := teamsPath(name, cfg)
	if p.PublicURL == "" {
		fmt.Printf("teams %q webhook path is %s\n", name, path)
		fmt.Printf("re-run with --public-url https://<host> to print the messaging endpoint to register\n")
		return nil
	}
	fmt.Printf("register this as the Azure Bot messaging endpoint (Azure Portal → your bot → Configuration):\n")
	fmt.Printf("  %s%s\n", strings.TrimRight(p.PublicURL, "/"), path)
	fmt.Printf("then enable the Teams channel and sideload the app package (docs/TEAMS-BOT-SETUP.md).\n")
	return nil
}

func (teamsKind) Detail(name string, cfg config.Transport) string {
	if id := cfg.Options["conversationId"]; id != "" {
		return "conversation=" + id
	}
	return ""
}

// Lane: an agent's teams lane is its OWN bot — App ID (by NAME via --user-env), client
// secret (by NAME via --token-env), and a default conversation.
func (teamsKind) Lane(name string, p LaneParams, _ map[string]config.Transport) (config.Transport, []string, error) {
	if p.TokenEnv == "" {
		return config.Transport{}, nil, fmt.Errorf("teams lanes need a bot: pass --token-env NAME (the client secret) and set the App ID via options")
	}
	var opts map[string]string
	if p.ChatID != "" {
		opts = map[string]string{"conversationId": p.ChatID}
	}
	want := config.Transport{Enabled: true, Kind: "teams", TokenEnv: p.TokenEnv, Options: opts}
	return want, []string{fmt.Sprintf("channel %q → teams bot (secret $%s) — next: messenger channel connect %s --public-url https://<host>", name, p.TokenEnv, name)}, nil
}

// teamsPath is the channel's inbound mount (options["path"] override; default shared).
func teamsPath(name string, cfg config.Transport) string {
	if p := cfg.Options["path"]; p != "" {
		return p
	}
	return "/webhook/teams"
}

// teamsChannel is one Teams bot: inbound via the Bot Framework Activity webhook
// (Teams/the Bot Connector POSTs Activities, signed with a Bot Connector JWT we verify
// against the OpenID metadata), outbound via the Bot Connector conversations REST API
// authenticated with an AAD client-credentials token. Send + inbound share this ONE
// object, so the last-seen serviceUrl persists in memory for proactive sends. Secrets
// (App ID, client secret) are resolved by NAME at the point of use — never logged.
type teamsChannel struct {
	name string
	cfg  config.Transport
	res  *SecretResolver

	mu         sync.Mutex
	tokenCache string
	tokenExp   time.Time
	serviceURL string // last-seen (from inbound) — persisted so proactive Send works
}

func openTeams(name string, cfg config.Transport, res *SecretResolver) (Channel, error) {
	return &teamsChannel{name: name, cfg: cfg, res: res, serviceURL: cfg.Options["serviceUrl"]}, nil
}

func (c *teamsChannel) Name() string { return c.name }
func (c *teamsChannel) Kind() string { return "teams" }
func (c *teamsChannel) Path() string { return teamsPath(c.name, c.cfg) }

// loginURL is the AAD token endpoint. options["loginURL"] lets tests point it at a
// local server. Single-tenant (tenantId set) uses the AAD tenant token endpoint;
// otherwise the Bot Framework multi-tenant endpoint.
func (c *teamsChannel) loginURL() string {
	if u := c.cfg.Options["loginURL"]; u != "" {
		return u
	}
	if t := c.cfg.Options["tenantId"]; t != "" {
		return "https://login.microsoftonline.com/" + t + "/oauth2/v2.0/token"
	}
	return "https://login.microsoftonline.com/botframework.com/oauth2/v2.0/token"
}

// openIDMetadataURL is the Bot Connector OpenID configuration document; options
// ["openIDMetadata"] lets tests point it (and thus the jwks_uri) at a local server.
func (c *teamsChannel) openIDMetadataURL() string {
	if u := c.cfg.Options["openIDMetadata"]; u != "" {
		return u
	}
	return "https://login.botframework.com/v1/.well-known/openidconfiguration"
}

// aadToken returns a cached AAD client-credentials token for the Bot Connector, minting
// a fresh one when the cache is empty or near expiry. The App ID and client secret are
// resolved by NAME and consumed ONLY inside the form body — never logged or returned.
func (c *teamsChannel) aadToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.tokenCache != "" && time.Now().Before(c.tokenExp) {
		tok := c.tokenCache
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	appID, err := c.res.User(c.cfg)
	if err != nil {
		return "", err
	}
	secret, err := c.res.Token(c.cfg)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {appID},
		"client_secret": {secret},
		"scope":         {"https://api.botframework.com/.default"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.loginURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("channel: teams AAD token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("channel: teams AAD token: status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("channel: teams AAD token: bad response")
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	c.mu.Lock()
	c.tokenCache = out.AccessToken
	c.tokenExp = time.Now().Add(ttl - time.Minute) // refresh a minute early
	c.mu.Unlock()
	return out.AccessToken, nil
}

// teamsActivity is the slice of a Bot Framework Activity we read (inbound) or write
// (outbound). Only fields messenger uses are modeled.
type teamsActivity struct {
	Type         string             `json:"type"`
	ID           string             `json:"id,omitempty"`
	Text         string             `json:"text,omitempty"`
	ServiceURL   string             `json:"serviceUrl,omitempty"`
	From         *teamsAccount      `json:"from,omitempty"`
	Conversation *teamsConversation `json:"conversation,omitempty"`
	ReplyToID    string             `json:"replyToId,omitempty"`
	Attachments  []teamsAttachment  `json:"attachments,omitempty"`
}

type teamsAccount struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type teamsConversation struct {
	ID string `json:"id,omitempty"`
}

// teamsAttachment is a Bot Framework attachment. For channel file downloads Teams sends
// contentType "application/vnd.microsoft.teams.file.download.info" whose content carries
// a downloadUrl; for hosted/image content the contentUrl is populated directly.
type teamsAttachment struct {
	ContentType string          `json:"contentType,omitempty"`
	ContentURL  string          `json:"contentUrl,omitempty"`
	Name        string          `json:"name,omitempty"`
	Content     json.RawMessage `json:"content,omitempty"`
}

// downloadURL returns the URL to fetch this attachment's bytes: the direct contentUrl,
// else the file.download.info content's downloadUrl.
func (a teamsAttachment) downloadURL() string {
	if a.ContentURL != "" {
		return a.ContentURL
	}
	if len(a.Content) > 0 {
		var c struct {
			DownloadURL string `json:"downloadUrl"`
		}
		if json.Unmarshal(a.Content, &c) == nil {
			return c.DownloadURL
		}
	}
	return ""
}

// attachmentType maps a Bot Framework contentType to the envelope attachment type.
func teamsAttachmentType(contentType string) string {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return "image"
	case strings.HasPrefix(contentType, "video/"):
		return "video"
	case strings.HasPrefix(contentType, "audio/"):
		return "audio"
	default:
		return "document"
	}
}

func (c *teamsChannel) Handler(pub Publisher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		// Verify the Bot Connector JWT unless explicitly disabled for local/dev behind a
		// trusted proxy (options["insecureSkipJWT"] = "true").
		if c.cfg.Options["insecureSkipJWT"] != "true" {
			if err := c.verifyInboundJWT(r.Context(), r.Header.Get("Authorization")); err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		var act teamsActivity
		if err := json.Unmarshal(body, &act); err != nil {
			http.Error(w, "bad activity", http.StatusBadRequest)
			return
		}
		// Persist the per-activity serviceUrl so a later proactive Send can reach Teams.
		if act.ServiceURL != "" {
			c.mu.Lock()
			c.serviceURL = act.ServiceURL
			c.mu.Unlock()
		}
		// Only message activities carry user text/media; ack everything else (typing,
		// conversationUpdate, …) without publishing.
		if act.Type != "" && act.Type != "message" {
			w.WriteHeader(http.StatusOK)
			return
		}
		sender := ""
		if act.From != nil {
			sender = act.From.Name
			if sender == "" {
				sender = act.From.ID
			}
		}
		env := envelope.Inbound(c.name, sender, act.Text, "Teams")
		env.Account = c.cfg.Account
		if act.ID != "" {
			env.ID = act.ID
		}
		if act.Conversation != nil {
			env.ThreadID = act.Conversation.ID
		}
		for _, a := range act.Attachments {
			du := a.downloadURL()
			if du == "" {
				continue
			}
			att := envelope.Attachment{Type: teamsAttachmentType(a.ContentType), Name: a.Name, MIME: a.ContentType}
			// Best-effort download: on ANY failure the envelope still ships with the
			// metadata-only attachment — a message is never dropped.
			if path, size, err := c.download(r.Context(), du, env.ID, a.Name); err == nil {
				att.Path = path
				att.Size = size
			} else {
				att.URL = du
			}
			env.Attachments = append(env.Attachments, att)
		}
		pub(env)
		w.WriteHeader(http.StatusOK)
	})
}

// download fetches one attachment (bearer the AAD token) and stores it under
// home.MediaDir() as "<id>-<name>". The token is consumed only in the request header.
func (c *teamsChannel) download(ctx context.Context, contentURL, id, name string) (string, int64, error) {
	token, err := c.aadToken(ctx)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, contentURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("channel: teams media fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", 0, fmt.Errorf("channel: teams media fetch: status %d", resp.StatusCode)
	}
	dir := home.MediaDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, fmt.Errorf("channel: teams media dir: %w", err)
	}
	dest := filepath.Join(dir, id+"-"+sanitizeFilename(name, "", id))
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("channel: teams media save: %w", err)
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, fmt.Errorf("channel: teams media save: %w", err)
	}
	return dest, n, nil
}

// Send posts a message Activity to the target conversation via the Bot Connector REST
// API, authenticated with the cached AAD token, and returns the Activity id Teams
// assigned. Text is sent first; URL-bearing attachments ride as attachment entries
// (contentType + contentUrl). ThreadID overrides the configured conversationId.
func (c *teamsChannel) Send(ctx context.Context, env envelope.Envelope) (string, error) {
	convID := env.ThreadID
	if convID == "" {
		convID = c.cfg.Options["conversationId"]
	}
	if convID == "" {
		return "", fmt.Errorf("channel: teams %q: no target (pass --to or configure --conversation)", c.name)
	}
	c.mu.Lock()
	serviceURL := c.serviceURL
	c.mu.Unlock()
	if serviceURL == "" {
		return "", fmt.Errorf("channel: teams %q: no serviceUrl yet (set options.serviceUrl or wait for an inbound message)", c.name)
	}
	token, err := c.aadToken(ctx)
	if err != nil {
		return "", err
	}

	act := teamsActivity{Type: "message", Text: env.Text}
	if env.ReplyTo != "" {
		act.ReplyToID = env.ReplyTo
	}
	for _, a := range env.Attachments {
		ref := a.URL
		if ref == "" {
			// A local-only file has no public URL the Bot Connector can fetch; surface
			// it as a link only when a publicURL is configured to host the media dir.
			if a.Path != "" && c.cfg.Options["publicURL"] != "" {
				ref = strings.TrimRight(c.cfg.Options["publicURL"], "/") + "/media/" + filepath.Base(a.Path)
			}
		}
		if ref == "" {
			continue
		}
		ct := a.MIME
		if ct == "" {
			ct = "application/octet-stream"
		}
		act.Attachments = append(act.Attachments, teamsAttachment{ContentType: ct, ContentURL: ref, Name: a.Name})
	}

	payload, _ := json.Marshal(act)
	endpoint := strings.TrimRight(serviceURL, "/") + "/v3/conversations/" + url.PathEscape(convID) + "/activities"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("channel: teams deliver: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("channel: teams deliver: status %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &out) == nil && out.ID != "" {
		return out.ID, nil
	}
	return "", nil
}

// --- Bot Connector JWT verification -----------------------------------------------

// botConnectorIssuer is the required token issuer for inbound Bot Connector JWTs.
const botConnectorIssuer = "https://api.botframework.com"

// verifyInboundJWT validates the Authorization: Bearer token on an inbound Activity
// against the Bot Connector OpenID metadata: RS256 signature over the JWKS key named by
// the header kid, issuer == https://api.botframework.com, audience == our App ID, and a
// non-expired token. The App ID is resolved by NAME. Returns nil only when all hold.
func (c *teamsChannel) verifyInboundJWT(ctx context.Context, authHeader string) error {
	appID, err := c.res.User(c.cfg)
	if err != nil {
		return err
	}
	raw := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if raw == "" {
		return fmt.Errorf("channel: teams jwt: missing bearer token")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return fmt.Errorf("channel: teams jwt: malformed")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := jwtUnmarshalSegment(parts[0], &hdr); err != nil {
		return fmt.Errorf("channel: teams jwt: bad header")
	}
	if hdr.Alg != "RS256" {
		return fmt.Errorf("channel: teams jwt: unexpected alg %q", hdr.Alg)
	}
	var claims struct {
		Iss string `json:"iss"`
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
	}
	if err := jwtUnmarshalSegment(parts[1], &claims); err != nil {
		return fmt.Errorf("channel: teams jwt: bad claims")
	}
	if claims.Iss != botConnectorIssuer {
		return fmt.Errorf("channel: teams jwt: issuer mismatch")
	}
	if claims.Aud != appID {
		return fmt.Errorf("channel: teams jwt: audience mismatch")
	}
	if claims.Exp != 0 && time.Now().After(time.Unix(claims.Exp, 0)) {
		return fmt.Errorf("channel: teams jwt: expired")
	}
	pub, err := c.jwksKey(ctx, hdr.Kid)
	if err != nil {
		return err
	}
	signed := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("channel: teams jwt: bad signature encoding")
	}
	h := sha256.Sum256([]byte(signed))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig); err != nil {
		return fmt.Errorf("channel: teams jwt: signature invalid")
	}
	return nil
}

// jwksKey resolves the RSA public key with the given kid from the Bot Connector JWKS
// (fetched via the OpenID metadata's jwks_uri).
func (c *teamsChannel) jwksKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	metaURL := c.openIDMetadataURL()
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := getJSON(ctx, metaURL, &meta); err != nil {
		return nil, fmt.Errorf("channel: teams jwt: openid metadata: %w", err)
	}
	if meta.JWKSURI == "" {
		return nil, fmt.Errorf("channel: teams jwt: no jwks_uri")
	}
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := getJSON(ctx, meta.JWKSURI, &jwks); err != nil {
		return nil, fmt.Errorf("channel: teams jwt: jwks: %w", err)
	}
	for _, k := range jwks.Keys {
		if k.Kid != kid {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("channel: teams jwt: bad modulus")
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("channel: teams jwt: bad exponent")
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nb),
			E: int(new(big.Int).SetBytes(eb).Int64()),
		}, nil
	}
	return nil, fmt.Errorf("channel: teams jwt: no key for kid")
}

// jwtUnmarshalSegment base64url-decodes a JWT segment (no padding) into v.
func jwtUnmarshalSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// getJSON GETs url and decodes the JSON body into v.
func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
