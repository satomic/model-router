#!/usr/bin/env bash
# Build the Go backend into server-go/bin/model-router.
#
# When frontend/dist exists it is copied into internal/web/dist first, so the binary carries the
# console and runs with nothing beside it. The server still prefers a frontend/dist on disk when
# there is one, so a `npm run build` shows up without recompiling.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
cd "$HERE"

EMBED="internal/web/dist"
rm -rf "$EMBED"
mkdir -p "$EMBED"
if [[ -d "$ROOT/frontend/dist" ]]; then
  cp -R "$ROOT/frontend/dist/." "$EMBED/"
  echo "==> embedded the console from frontend/dist"
else
  echo "==> no frontend/dist found; building the API only"
fi
# Always, console or not. `go:embed all:dist` refuses a directory that does not exist, so this
# placeholder is what lets a fresh clone -- which carries no built console -- compile at all.
# It is the one file in here that git tracks; see .gitignore.
touch "$EMBED/.gitkeep"

mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/model-router ./cmd/model-router
echo "==> built $HERE/bin/model-router"
