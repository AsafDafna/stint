#!/bin/zsh
# Builds the brain against the libsqlite3 macOS ships (not Homebrew's) and installs it.
# go-sqlite3's darwin cgo flags point at /opt/homebrew/opt/sqlite first, so the SDK paths
# are put in front, and sqlite_omit_load_extension is required because Apple's library
# does not export sqlite3_load_extension.
set -euo pipefail
HERE="${0:A:h}"
SDK="$(xcrun --show-sdk-path)"
OUT="${1:-$HOME/.local/bin/stint}"
mkdir -p "$(dirname "$OUT")"
CGO_CFLAGS="-I$SDK/usr/include" CGO_LDFLAGS="-L$SDK/usr/lib" \
  go -C "$HERE" build -tags 'libsqlite3,sqlite_omit_load_extension' -o "$OUT" ./cmd/stint
# Anchored: a Homebrew path merely containing the substring must not pass.
otool -L "$OUT" | grep -Eq '^[[:space:]]*/usr/lib/libsqlite3' || { echo "not linked against the system libsqlite3" >&2; exit 1; }
echo "built $OUT"
