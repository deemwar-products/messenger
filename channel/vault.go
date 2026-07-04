package channel

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/deemwar-products/messenger/config"
	"github.com/deemwar-products/messenger/home"
)

// Vault decrypts named secret entries host-only. Entries live at vault/<name>.age
// (age-encrypted to the host recipient); the host-only identity lives at
// keys/vault.key. Reveal is the ONLY exit for a value and callers use it at the point
// of consumption — the value is never logged, written back to config, or placed on the
// wire.
type Vault struct {
	dir     string
	keyPath string
}

// DefaultVault points at the messenger home: vault/ for ciphertext, keys/vault.key for
// the identity.
func DefaultVault() *Vault {
	return &Vault{dir: home.Path("vault"), keyPath: home.Path("keys", "vault.key")}
}

// NewVault points at an explicit dir/key (used by tests).
func NewVault(dir, keyPath string) *Vault { return &Vault{dir: dir, keyPath: keyPath} }

func (v *Vault) identities() ([]age.Identity, error) {
	f, err := os.Open(v.keyPath)
	if err != nil {
		return nil, fmt.Errorf("channel: vault key: %w", err)
	}
	defer f.Close()
	return age.ParseIdentities(f)
}

// Reveal decrypts vault/<name>.age and returns the plaintext. USE ONLY at the point a
// command consumes it; never log or persist the result. Errors carry the entry NAME,
// never its contents.
func (v *Vault) Reveal(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("channel: vault: empty entry name")
	}
	ids, err := v.identities()
	if err != nil {
		return "", err
	}
	ct, err := os.ReadFile(filepath.Join(v.dir, name+".age"))
	if err != nil {
		return "", fmt.Errorf("channel: vault entry %q: %w", name, err)
	}
	r, err := age.Decrypt(bytes.NewReader(ct), ids...)
	if err != nil {
		return "", fmt.Errorf("channel: vault entry %q: decrypt failed", name)
	}
	pt, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("channel: vault entry %q: read failed", name)
	}
	return strings.TrimRight(string(pt), "\n"), nil
}

// Seal writes value to vault/<name>.age encrypted to recipient. value is consumed here
// and not retained; the ciphertext at rest never contains it.
func (v *Vault) Seal(name, value string, recipient age.Recipient) error {
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return err
	}
	var ct bytes.Buffer
	w, err := age.Encrypt(&ct, recipient)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, value); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(v.dir, name+".age"), ct.Bytes(), 0o600)
}

// SecretResolver resolves a connection's secret by NAME at the point of use: the age
// vault entry named by TokenVault if set, else the env var named by TokenEnv. Neither
// the config shape nor this resolver ever holds the value at rest.
type SecretResolver struct {
	vault *Vault
}

// NewSecretResolver returns a resolver over v (nil = DefaultVault at first use).
func NewSecretResolver(v *Vault) *SecretResolver { return &SecretResolver{vault: v} }

func (s *SecretResolver) v() *Vault {
	if s.vault == nil {
		s.vault = DefaultVault()
	}
	return s.vault
}

// Token resolves the token secret for cfg (TokenVault, else TokenEnv).
func (s *SecretResolver) Token(cfg config.Transport) (string, error) {
	return s.resolve("token", cfg.TokenVault, cfg.TokenEnv)
}

// User resolves the user/identity secret for cfg (UserVault, else UserEnv).
func (s *SecretResolver) User(cfg config.Transport) (string, error) {
	return s.resolve("user", cfg.UserVault, cfg.UserEnv)
}

func (s *SecretResolver) resolve(what, vaultName, envName string) (string, error) {
	if vaultName != "" {
		return s.v().Reveal(vaultName)
	}
	if envName != "" {
		val := os.Getenv(envName)
		if val == "" {
			return "", fmt.Errorf("channel: %s secret: env %q is unset", what, envName)
		}
		return val, nil
	}
	return "", fmt.Errorf("channel: %s secret: no source (set tokenVault or tokenEnv)", what)
}
