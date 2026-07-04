#!/usr/bin/env bash
# Install the messenger agent skill by symlinking skills/messenger into the agent skill
# dirs. Idempotent: re-running retargets the symlink. The symlink is per-machine
# activation; the tracked source of truth is skills/messenger in this repo.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO_ROOT/skills/messenger"

link_into() {
  local dir="$1"
  [ -d "$dir" ] || mkdir -p "$dir"
  local dest="$dir/messenger"
  if [ -L "$dest" ] || [ -e "$dest" ]; then rm -rf "$dest"; fi
  ln -s "$SRC" "$dest"
  echo "linked $dest -> $SRC"
}

# Global (all sessions) and repo-local (this project) activation.
link_into "$HOME/.claude/skills"
link_into "$REPO_ROOT/.claude/skills"

echo "messenger skill installed. Restart the agent session to pick it up."
