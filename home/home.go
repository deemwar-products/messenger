// Package home resolves messenger's single on-disk state directory. It honors
// MESSENGER_HOME, else ~/.config/messenger. The config file, the inbox, and the age
// vault/keys live under it.
package home

import (
	"os"
	"path/filepath"
)

// Dir returns the messenger home directory: MESSENGER_HOME, else ~/.config/messenger on
// EVERY platform. We deliberately do NOT use os.UserConfigDir() — on macOS that resolves
// to ~/Library/Application Support, which would silently disagree with the documented and
// deployed ~/.config/messenger (and, e.g., an OS service started without MESSENGER_HOME
// would read a different, empty home than the CLI). One home path, all platforms.
func Dir() string {
	if h := os.Getenv("MESSENGER_HOME"); h != "" {
		return h
	}
	hd, err := os.UserHomeDir()
	if err != nil {
		hd = os.Getenv("HOME")
	}
	return filepath.Join(hd, ".config", "messenger")
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
