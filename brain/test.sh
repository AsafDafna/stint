#!/bin/zsh
# Runs the tests linked against the same libsqlite3 as the installed binary (see build.sh).
# Usage: zsh brain/test.sh [go test args…]   (default: ./...)
set -euo pipefail
HERE="${0:A:h}"
SDK="$(xcrun --show-sdk-path)"
export CGO_CFLAGS="-I$SDK/usr/include" CGO_LDFLAGS="-L$SDK/usr/lib"
if (( $# == 0 )); then set -- ./...; fi
exec go -C "$HERE" test -tags 'libsqlite3,sqlite_omit_load_extension' "$@"
