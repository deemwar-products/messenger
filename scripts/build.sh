#!/usr/bin/env bash
# build.sh — the reliable local build for the single static messenger binary.
#
# WHY THIS EXISTS: on macOS (Apple Silicon especially), overwriting a binary the kernel has
# already seen invalidates its cached ad-hoc code signature, and the very next run dies with
# "Killed: 9" (SIGKILL) before main() — a rebuild that mysteriously won't start. The fix is
# to strip quarantine/extended attributes and re-apply an ad-hoc signature after every build.
# This is a no-op on Linux, where the static CGO_ENABLED=0 binary needs no signing.
#
# There is no Taskfile in this repo (the CLI is the interface); this is the one dev build helper.
#
#   scripts/build.sh            # build ./messenger, green-gate, and (on macOS) re-sign
#   scripts/build.sh -o /path   # write the binary elsewhere
set -euo pipefail
cd "$(dirname "$0")/.."

out="./messenger"
if [ "${1:-}" = "-o" ] && [ -n "${2:-}" ]; then out="$2"; fi

echo "==> build (CGO_ENABLED=0)"
CGO_ENABLED=0 go build -o "$out" ./cmd/messenger

echo "==> vet"
go vet ./...

if [ "$(uname -s)" = "Darwin" ]; then
  echo "==> macOS: clear xattrs + ad-hoc codesign (avoids Killed: 9 after rebuild)"
  xattr -c "$out" 2>/dev/null || true
  codesign --force --sign - "$out"
  codesign --verify --verbose=1 "$out" 2>&1 | sed 's/^/    /' || true
fi

echo "==> ok: $out"
