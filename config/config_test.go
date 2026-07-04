package config

import (
	"path/filepath"
	"testing"
)

// Save then Load round-trips many channels of different kinds keyed by name, including a
// per-channel default target (chatId) — proving multiple channels coexist.
func TestConfig_SaveLoadMultipleChannels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := &Config{
		ServeTokenEnv: "MESSENGER_SERVE_TOKEN",
		Transports: map[string]Transport{
			"home":  {Enabled: true, Kind: "whatsapp"},
			"mybot": {Enabled: true, Kind: "telegram", TokenEnv: "TELEGRAM_BOT_TOKEN", Options: map[string]string{"chatId": "-100123"}},
			"other": {Enabled: false, Kind: "telegram", TokenEnv: "OTHER_TOKEN", Options: map[string]string{"chatId": "-100999"}},
			"in":    {Enabled: true, Kind: "hook", TokenEnv: "MESSENGER_HOOK_SECRET"},
		},
	}
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Transports) != 4 {
		t.Fatalf("want 4 channels, got %d", len(got.Transports))
	}
	if got.Transports["mybot"].Options["chatId"] != "-100123" {
		t.Fatalf("mybot chatId lost: %+v", got.Transports["mybot"])
	}
	// Two telegram channels coexist; only enabled ones surface in Enabled().
	en := got.Enabled()
	if _, ok := en["other"]; ok {
		t.Fatal("disabled channel leaked into Enabled()")
	}
	if len(en) != 3 {
		t.Fatalf("want 3 enabled, got %d", len(en))
	}
}
