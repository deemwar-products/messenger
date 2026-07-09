package channel

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/envelope"
)

// teamsTestCfg builds a teams Transport wired to env-var NAMES only (never values).
func teamsTestCfg(opts map[string]string) config.Transport {
	return config.Transport{
		Enabled:  true,
		Kind:     "teams",
		TokenEnv: "TEAMS_TEST_SECRET",
		UserEnv:  "TEAMS_TEST_APPID",
		Options:  opts,
	}
}

func TestTeamsKindRegistered(t *testing.T) {
	k, ok := Kinds()["teams"]
	if !ok {
		t.Fatal("teams kind not registered")
	}
	if k.Traits().TargetFlag != "conversation" || !k.Traits().RequiresToken {
		t.Fatalf("unexpected traits: %+v", k.Traits())
	}
	if d := k.Detail("t", teamsTestCfg(map[string]string{"conversationId": "19:abc"})); d != "conversation=19:abc" {
		t.Fatalf("detail = %q", d)
	}
}

func TestTeamsActivityParseText(t *testing.T) {
	body := `{"type":"message","id":"act-1","text":"hello desk",
		"serviceUrl":"https://smba.example/","from":{"id":"29:u","name":"Muthu"},
		"conversation":{"id":"19:chan@thread.tacv2"}}`
	var act teamsActivity
	if err := json.Unmarshal([]byte(body), &act); err != nil {
		t.Fatal(err)
	}
	if act.Type != "message" || act.Text != "hello desk" || act.ID != "act-1" {
		t.Fatalf("bad parse: %+v", act)
	}
	if act.From.Name != "Muthu" || act.Conversation.ID != "19:chan@thread.tacv2" {
		t.Fatalf("bad from/conv: %+v", act)
	}
	if act.ServiceURL != "https://smba.example/" {
		t.Fatalf("bad serviceUrl: %q", act.ServiceURL)
	}
}

func TestTeamsActivityParseAttachment(t *testing.T) {
	body := `{"type":"message","attachments":[
		{"contentType":"image/png","contentUrl":"https://smba.example/img.png","name":"img.png"},
		{"contentType":"application/vnd.microsoft.teams.file.download.info","name":"report.pdf",
		 "content":{"downloadUrl":"https://sharepoint.example/report.pdf"}}]}`
	var act teamsActivity
	if err := json.Unmarshal([]byte(body), &act); err != nil {
		t.Fatal(err)
	}
	if len(act.Attachments) != 2 {
		t.Fatalf("want 2 attachments, got %d", len(act.Attachments))
	}
	if got := act.Attachments[0].downloadURL(); got != "https://smba.example/img.png" {
		t.Fatalf("direct contentUrl = %q", got)
	}
	if teamsAttachmentType(act.Attachments[0].ContentType) != "image" {
		t.Fatalf("want image type")
	}
	if got := act.Attachments[1].downloadURL(); got != "https://sharepoint.example/report.pdf" {
		t.Fatalf("file.download.info downloadUrl = %q", got)
	}
	if teamsAttachmentType(act.Attachments[1].ContentType) != "document" {
		t.Fatalf("want document type")
	}
}

func TestTeamsAADTokenRequestShape(t *testing.T) {
	t.Setenv("TEAMS_TEST_SECRET", "shhh-not-real")
	t.Setenv("TEAMS_TEST_APPID", "app-id-123")

	var gotForm url.Values
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(b))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"AAD-TOKEN","expires_in":3600}`)
	}))
	defer srv.Close()

	res := NewSecretResolver(nil)
	ch, err := openTeams("t", teamsTestCfg(map[string]string{"loginURL": srv.URL}), res)
	if err != nil {
		t.Fatal(err)
	}
	tc := ch.(*teamsChannel)
	tok, err := tc.aadToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "AAD-TOKEN" {
		t.Fatalf("token = %q", tok)
	}
	if !strings.HasPrefix(gotCT, "application/x-www-form-urlencoded") {
		t.Fatalf("content-type = %q", gotCT)
	}
	if gotForm.Get("grant_type") != "client_credentials" {
		t.Fatalf("grant_type = %q", gotForm.Get("grant_type"))
	}
	if gotForm.Get("client_id") != "app-id-123" {
		t.Fatalf("client_id = %q", gotForm.Get("client_id"))
	}
	if gotForm.Get("scope") != "https://api.botframework.com/.default" {
		t.Fatalf("scope = %q", gotForm.Get("scope"))
	}

	// Second call is served from cache (server not hit again → still valid token).
	gotForm = nil
	tok2, err := tc.aadToken(context.Background())
	if err != nil || tok2 != "AAD-TOKEN" {
		t.Fatalf("cached token = %q err=%v", tok2, err)
	}
	if gotForm != nil {
		t.Fatalf("expected cache hit, but token endpoint was called again")
	}
}

func TestTeamsSendOutboundActivityJSON(t *testing.T) {
	t.Setenv("TEAMS_TEST_SECRET", "shhh-not-real")
	t.Setenv("TEAMS_TEST_APPID", "app-id-123")

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"AAD-TOKEN","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	var gotPath, gotAuth string
	var gotAct teamsActivity
	convSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotAct)
		io.WriteString(w, `{"id":"posted-42"}`)
	}))
	defer convSrv.Close()

	res := NewSecretResolver(nil)
	ch, err := openTeams("t", teamsTestCfg(map[string]string{
		"loginURL":       tokenSrv.URL,
		"serviceUrl":     convSrv.URL,
		"conversationId": "19:chan@thread.tacv2",
	}), res)
	if err != nil {
		t.Fatal(err)
	}

	env := envelope.Envelope{
		Channel: "t",
		Text:    "status: green",
		Attachments: []envelope.Attachment{
			{Type: "image", MIME: "image/png", URL: "https://cdn.example/a.png", Name: "a.png"},
		},
	}
	id, err := ch.Send(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if id != "posted-42" {
		t.Fatalf("send id = %q", id)
	}
	if gotAuth != "Bearer AAD-TOKEN" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotPath, "/v3/conversations/") || !strings.HasSuffix(gotPath, "/activities") {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAct.Type != "message" || gotAct.Text != "status: green" {
		t.Fatalf("activity = %+v", gotAct)
	}
	if len(gotAct.Attachments) != 1 || gotAct.Attachments[0].ContentURL != "https://cdn.example/a.png" {
		t.Fatalf("attachments = %+v", gotAct.Attachments)
	}
	if gotAct.Attachments[0].ContentType != "image/png" {
		t.Fatalf("contentType = %q", gotAct.Attachments[0].ContentType)
	}
}

func TestTeamsSendNoServiceURL(t *testing.T) {
	res := NewSecretResolver(nil)
	ch, _ := openTeams("t", teamsTestCfg(map[string]string{"conversationId": "19:x"}), res)
	_, err := ch.Send(context.Background(), envelope.Envelope{Channel: "t", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "serviceUrl") {
		t.Fatalf("want serviceUrl error, got %v", err)
	}
}

// --- JWT verification tests -------------------------------------------------------

// signTestJWT builds an RS256 JWT with the given claims and kid, signed by key.
func signTestJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hdr := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(hdr) + "." + enc(claims)
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// jwksServer serves an OpenID metadata doc + JWKS exposing key under kid.
func jwksServer(t *testing.T, key *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/openid", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"jwks_uri": base + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{"kid": kid, "n": n, "e": e, "kty": "RSA"}},
		})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	return srv
}

func TestTeamsVerifyInboundJWT(t *testing.T) {
	t.Setenv("TEAMS_TEST_APPID", "app-id-123")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := jwksServer(t, key, "kid-1")
	defer srv.Close()

	res := NewSecretResolver(nil)
	ch, _ := openTeams("t", teamsTestCfg(map[string]string{"openIDMetadata": srv.URL + "/openid"}), res)
	tc := ch.(*teamsChannel)
	ctx := context.Background()

	good := signTestJWT(t, key, "kid-1", map[string]any{
		"iss": botConnectorIssuer, "aud": "app-id-123", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if err := tc.verifyInboundJWT(ctx, "Bearer "+good); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}

	// Wrong audience → rejected.
	badAud := signTestJWT(t, key, "kid-1", map[string]any{
		"iss": botConnectorIssuer, "aud": "someone-else", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if err := tc.verifyInboundJWT(ctx, "Bearer "+badAud); err == nil {
		t.Fatal("wrong audience accepted")
	}

	// Wrong issuer → rejected.
	badIss := signTestJWT(t, key, "kid-1", map[string]any{
		"iss": "https://evil.example", "aud": "app-id-123", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if err := tc.verifyInboundJWT(ctx, "Bearer "+badIss); err == nil {
		t.Fatal("wrong issuer accepted")
	}

	// Expired → rejected.
	expired := signTestJWT(t, key, "kid-1", map[string]any{
		"iss": botConnectorIssuer, "aud": "app-id-123", "exp": time.Now().Add(-time.Hour).Unix(),
	})
	if err := tc.verifyInboundJWT(ctx, "Bearer "+expired); err == nil {
		t.Fatal("expired token accepted")
	}

	// Signature by a different key → rejected.
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := signTestJWT(t, otherKey, "kid-1", map[string]any{
		"iss": botConnectorIssuer, "aud": "app-id-123", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if err := tc.verifyInboundJWT(ctx, "Bearer "+forged); err == nil {
		t.Fatal("forged signature accepted")
	}

	// Missing bearer → rejected.
	if err := tc.verifyInboundJWT(ctx, ""); err == nil {
		t.Fatal("missing token accepted")
	}
}

// TestTeamsHandlerInbound drives a full inbound Activity through the Pushed handler with
// JWT verification skipped (insecure dev mode) and asserts the published envelope +
// serviceUrl persistence.
func TestTeamsHandlerInbound(t *testing.T) {
	res := NewSecretResolver(nil)
	ch, _ := openTeams("t", teamsTestCfg(map[string]string{"insecureSkipJWT": "true"}), res)
	tc := ch.(*teamsChannel)

	var got envelope.Envelope
	h := tc.Handler(func(e envelope.Envelope) { got = e })

	body := `{"type":"message","id":"act-9","text":"ping","serviceUrl":"https://smba.example/",
		"from":{"id":"29:u","name":"Muthu"},"conversation":{"id":"19:c@thread.tacv2"}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/teams", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got.Text != "ping" || got.Sender != "Muthu" || got.ThreadID != "19:c@thread.tacv2" {
		t.Fatalf("envelope = %+v", got)
	}
	if got.ID != "act-9" || got.Origin != "Teams" {
		t.Fatalf("id/origin = %q/%q", got.ID, got.Origin)
	}
	// serviceUrl persisted for proactive Send.
	tc.mu.Lock()
	su := tc.serviceURL
	tc.mu.Unlock()
	if su != "https://smba.example/" {
		t.Fatalf("serviceUrl not persisted: %q", su)
	}

	// Non-message activity is acked without publishing.
	got = envelope.Envelope{}
	req2 := httptest.NewRequest(http.MethodPost, "/webhook/teams",
		strings.NewReader(`{"type":"conversationUpdate"}`))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK || got.Text != "" {
		t.Fatalf("conversationUpdate should ack-only: code=%d env=%+v", w2.Code, got)
	}
}

func TestTeamsValidateAndLane(t *testing.T) {
	k := teamsKind{}
	// Validate is permissive (Base default) — a fresh channel passes.
	if err := k.Validate("t", teamsTestCfg(nil), map[string]config.Transport{}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// Lane needs a token env.
	if _, _, err := k.Lane("t", LaneParams{}, nil); err == nil {
		t.Fatal("lane without token-env should error")
	}
	tr, hints, err := k.Lane("t", LaneParams{TokenEnv: "TEAMS_BOT_PASSWORD", ChatID: "19:x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Kind != "teams" || tr.TokenEnv != "TEAMS_BOT_PASSWORD" || tr.Options["conversationId"] != "19:x" {
		t.Fatalf("lane transport = %+v", tr)
	}
	if len(hints) == 0 {
		t.Fatal("want lane hints")
	}
}
