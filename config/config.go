// Package config is messenger's on-disk configuration: a TOML file naming the enabled
// channels and, per channel, its kind + account + secret NAMES + free-form options.
// A secret VALUE never lives here — only the NAME of the env var or age vault entry
// that holds it (resolved host-only at the point of use, see the transport vault).
package config

import (
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

// Config is the whole messenger config: a table of channel name -> Transport. The HTTP
// server bearer token is referenced by NAME via ServeTokenEnv (never the value).
type Config struct {
	// ServeTokenEnv names the env var holding the bearer token `messenger serve` requires
	// on POST /send. Empty = no auth (loopback-only dev).
	ServeTokenEnv string `toml:"serveTokenEnv"`
	// Transports maps a channel name (also the Envelope Channel) to its connection config.
	Transports map[string]Transport `toml:"transports"`
}

// Transport is one channel's connection config. Kind selects the adapter (defaults to
// the channel name). Secrets are referenced by NAME only.
type Transport struct {
	Enabled    bool              `toml:"enabled"`
	Kind       string            `toml:"kind"`       // connection kind; defaults to the channel name
	Account    string            `toml:"account"`    // platform account / workspace
	TokenEnv   string            `toml:"tokenEnv"`   // NAME of the env var holding the token
	UserEnv    string            `toml:"userEnv"`    // NAME of the env var holding the user/identity
	TokenVault string            `toml:"tokenVault"` // name of the age vault entry holding the token
	UserVault  string            `toml:"userVault"`  // name of the age vault entry holding the user/identity
	Options    map[string]string `toml:"options"`    // free-form per-kind options (never a secret value)
}

// Load reads and parses the TOML config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Transports == nil {
		c.Transports = map[string]Transport{}
	}
	return &c, nil
}

// Enabled returns the channel->Transport map filtered to enabled connections.
func (c *Config) Enabled() map[string]Transport {
	out := map[string]Transport{}
	for ch, t := range c.Transports {
		if t.Enabled {
			out[ch] = t
		}
	}
	return out
}
