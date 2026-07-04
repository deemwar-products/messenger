// Package skills embeds the portable agent skill (SKILL.md + references/) so ANY
// installed messenger binary can drop it into an agent's skill directory:
// `messenger install --skills`. The binary is the ONLY installer — no shell scripts.
// The tracked source of truth is skills/messenger/, compiled in at build time.
package skills

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed messenger
var skillFS embed.FS

// Install writes the embedded skill tree to <dir>/messenger/ (files 0644, dirs 0755)
// and returns the SKILL.md path. Idempotent: re-running overwrites with the binary's copy.
func Install(dir string) (string, error) {
	err := fs.WalkDir(skillFS, "messenger", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, path)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, rerr := skillFS.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(dest, data, 0o644)
	})
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "messenger", "SKILL.md"), nil
}
