// Package home resolves messenger's single on-disk state directory. It honors
// MESSENGER_HOME, else ~/.config/messenger. The config file, the inbox, and the age
// vault/keys live under it.
package home

import (
	"os"
	"path/filepath"
)

// Dir returns the messenger home directory (MESSENGER_HOME or ~/.config/messenger).
func Dir() string {
	if h := os.Getenv("MESSENGER_HOME"); h != "" {
		return h
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		cfg = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(cfg, "messenger")
}

// Path joins parts onto the home directory.
func Path(parts ...string) string {
	return filepath.Join(append([]string{Dir()}, parts...)...)
}

// ConfigPath is the canonical config file location.
func ConfigPath() string { return Path("config.toml") }

// InboxPath is the append-only inbound ndjson file.
func InboxPath() string { return Path("inbox.ndjson") }

// MediaDir is where inbound attachments are stored (served at GET /media/<basename>).
func MediaDir() string { return Path("media") }
