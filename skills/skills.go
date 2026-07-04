// Package skills embeds the portable agent skill so ANY installed messenger binary can
// drop it into an agent's skill directory: `messenger install --skills`. The binary is
// the ONLY installer — no shell scripts. The tracked source of truth is
// skills/messenger/SKILL.md, compiled in at build time.
package skills

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed messenger/SKILL.md
var skillMD []byte

// Install writes the embedded skill to <dir>/messenger/SKILL.md (0644, dirs 0755) and
// returns the written path. Idempotent: re-running overwrites with the binary's copy.
func Install(dir string) (string, error) {
	dest := filepath.Join(dir, "messenger")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dest, "SKILL.md")
	if err := os.WriteFile(path, skillMD, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
