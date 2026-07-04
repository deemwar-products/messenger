package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"

	"github.com/deemwar-products/messenger/config"
)

// Per-kind connectivity probes for `messenger channel test <name>`. Each returns
// human-readable lines; a secret VALUE never appears in them — only names and
// platform-side identities (bot username, linked JID).

// testTelegram calls the Bot API getMe with the channel's token (resolved by NAME at
// the point of the call), proving the token is set and valid, and reports the bot.
func testTelegram(ctx context.Context, name string, cfg config.Transport, res *SecretResolver) ([]string, error) {
	token, err := res.Token(cfg)
	if err != nil {
		return nil, fmt.Errorf("token not resolvable: %w", err)
	}
	base := cfg.Options["baseURL"]
	if base == "" {
		base = "https://api.telegram.org"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/bot"+token+"/getMe", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("getMe status %d (token invalid or revoked)", resp.StatusCode)
	}
	var out struct {
		Result struct {
			Username string `json:"username"`
			ID       int64  `json:"id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(body, &out)
	lines := []string{fmt.Sprintf("token OK — bot @%s (id %d)", out.Result.Username, out.Result.ID)}
	if id := cfg.Options["chatId"]; id != "" {
		lines = append(lines, "default target chat: "+id)
	} else {
		lines = append(lines, "no default --chat-id: every send needs --to")
	}
	return lines, nil
}

// testWhatsapp probes the GLOBAL device via wacli doctor and, when the channel is
// bound to a group, checks the group is known to the local store.
func testWhatsapp(ctx context.Context, name string, cfg config.Transport, _ *SecretResolver) ([]string, error) {
	bin := waBin(cfg)
	st := WhatsappDeviceStatus(ctx, bin)
	if !st.Installed {
		return nil, fmt.Errorf("%s not found on PATH — install it (https://wacli.sh)", bin)
	}
	if !st.Authenticated {
		return nil, fmt.Errorf("device not paired — run: messenger channel connect %s", name)
	}
	lines := []string{fmt.Sprintf("device linked: %s (global — serves every whatsapp channel)", st.LinkedJID)}
	if g := cfg.Options["group"]; g != "" {
		if known, gname := whatsappGroupKnown(ctx, bin, g); known {
			lines = append(lines, fmt.Sprintf("group %s known: %s", g, gname))
		} else {
			lines = append(lines, fmt.Sprintf("WARNING: group %s not in the local store — run `%s sync` or check the JID", g, bin))
		}
	} else {
		lines = append(lines, "no --group: this channel is the catch-all for unmatched chats")
	}
	return lines, nil
}

// whatsappGroupKnown checks the local wacli store for the group JID.
func whatsappGroupKnown(ctx context.Context, bin, jid string) (bool, string) {
	out, err := exec.CommandContext(ctx, bin, "--json", "groups", "list").Output()
	if err != nil {
		return false, ""
	}
	var res struct {
		Data []struct {
			JID  string `json:"jid"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if json.Unmarshal(out, &res) != nil {
		return false, ""
	}
	for _, g := range res.Data {
		if g.JID == jid {
			return true, g.Name
		}
	}
	return false, ""
}

// testWebhook verifies the HMAC secret is resolvable and reports the inbound path and
// outbound callback.
func testWebhook(ctx context.Context, name string, cfg config.Transport, res *SecretResolver) ([]string, error) {
	if _, err := res.Token(cfg); err != nil {
		return nil, fmt.Errorf("secret not resolvable: %w", err)
	}
	p := cfg.Options["path"]
	if p == "" {
		p = "/webhook/" + name
	}
	lines := []string{"secret OK (resolved by NAME, value never printed)", "inbound path: " + p}
	if cb := cfg.Options["callbackURL"]; cb != "" {
		lines = append(lines, "outbound callback: "+cb)
	} else {
		lines = append(lines, "no callbackURL: this channel is inbound-only")
	}
	return lines, nil
}
