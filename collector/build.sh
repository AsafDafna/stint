#!/bin/zsh
# Builds, signs and installs the stint collector as a minimal app bundle:
#   collector/.build/stint-collector.app  ->  ~/Applications/stint-collector.app
# TCC keys Accessibility/Automation grants by bundle id + signing identity, so the bundle and the identity must both be stable.
# Signing identity: $STINT_SIGN_IDENTITY, default "stint" (self-signed code-signing cert, see README).
set -euo pipefail
HERE="${0:A:h}"
BUILD="$HERE/.build"
APP="$BUILD/stint-collector.app"
INSTALL="$HOME/Applications/stint-collector.app"
IDENTITY="${STINT_SIGN_IDENTITY:-stint}"
mkdir -p "$BUILD" "$APP/Contents/MacOS"
"$HERE/gen-schema.sh" "$BUILD/Schema.swift"
swiftc -O -o "$APP/Contents/MacOS/stint-collector" \
  "$HERE/main.swift" "$HERE/Pure.swift" "$BUILD/Schema.swift" \
  -framework AppKit -framework CoreAudio -framework IOKit -lsqlite3
cp "$HERE/Info.plist" "$APP/Contents/Info.plist"
codesign -s "$IDENTITY" --force --timestamp=none "$APP"
codesign -dv "$APP" 2>&1 | grep -E '^(Identifier|Authority)='
mkdir -p "$HOME/Applications"
rm -rf "$INSTALL"
ditto "$APP" "$INSTALL"
echo "installed $INSTALL"
