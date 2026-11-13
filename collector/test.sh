#!/bin/zsh
# Compiles Pure.swift + the generated Schema.swift + PureTests.swift with swiftc and runs the assertions.
set -euo pipefail
HERE="${0:A:h}"
BUILD="$HERE/.build"
mkdir -p "$BUILD"
"$HERE/gen-schema.sh" "$BUILD/Schema.swift"
swiftc -O -parse-as-library -o "$BUILD/pure-tests" \
  "$HERE/Pure.swift" "$BUILD/Schema.swift" "$HERE/PureTests.swift" \
  -lsqlite3
"$BUILD/pure-tests"
