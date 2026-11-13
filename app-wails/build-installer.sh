#!/usr/bin/env bash
# Builds KopiaUI-Wails.exe and its NSIS installer.
#
# Usage (from Git Bash, in app-wails/):
#   ./build-installer.sh
#
# Environment:
#   KOPIA_EXE   kopia.exe to bundle (default: ../dist/kopia_windows_amd64/kopia.exe,
#               built by `make kopia-ui` or `make install` in the repository root)
#   MAKENSIS    makensis.exe (default: PATH, then electron-builder's NSIS cache)
#
# Output: ../dist/kopiaui-wails/ and ../dist/KopiaUI-Wails-Setup-<version>.exe
set -euo pipefail

cd "$(dirname "$0")"

DIST=../dist
OUT="$DIST/kopiaui-wails"
KOPIA_EXE=${KOPIA_EXE:-$DIST/kopia_windows_amd64/kopia.exe}

if [ ! -f "$KOPIA_EXE" ]; then
  echo "kopia.exe not found at $KOPIA_EXE; build it first or set KOPIA_EXE" >&2
  exit 1
fi

find_makensis() {
  if [ -n "${MAKENSIS:-}" ]; then echo "$MAKENSIS"; return; fi
  if command -v makensis >/dev/null 2>&1; then command -v makensis; return; fi
  local cache="${LOCALAPPDATA:-}/electron-builder/Cache/nsis"
  find "$cache" -path '*/Bin/makensis.exe' 2>/dev/null | sort | tail -1
}

MAKENSIS_EXE=$(find_makensis)
if [ -z "$MAKENSIS_EXE" ] || [ ! -f "$MAKENSIS_EXE" ]; then
  echo "makensis not found; install NSIS 3 or set MAKENSIS" >&2
  exit 1
fi

# Version scheme matches Kopia's own builds: <yyyymmdd>.0.0-<commit>.
DATE=$(date +%Y%m%d)
COMMIT=$(git rev-parse --short=8 HEAD)
if ! git diff --quiet HEAD -- . 2>/dev/null; then
  COMMIT="$COMMIT-dirty"
fi
VERSION="$DATE.0.0-$COMMIT"
# Windows file versions are four numbers of at most 65535 each.
VIVERSION="${DATE:0:4}.$((10#${DATE:4:4})).0.0"

echo "Building KopiaUI-Wails $VERSION"

go generate ./...
mkdir -p "$OUT"
go build -trimpath -ldflags "-s -w -H windowsgui -X main.buildVersion=$VERSION" -o "$OUT/KopiaUI-Wails.exe" .
cp "$KOPIA_EXE" "$OUT/kopia.exe"

SIZE_KB=$(( ( $(stat -c %s "$OUT/KopiaUI-Wails.exe") + $(stat -c %s "$OUT/kopia.exe") ) / 1024 ))
SETUP="$DIST/KopiaUI-Wails-Setup-$VERSION.exe"

"$MAKENSIS_EXE" -V2 \
  "-DVERSION=$VERSION" \
  "-DVIVERSION=$VIVERSION" \
  "-DSRCDIR=$(cygpath -w "$OUT")" \
  "-DOUTFILE=$(cygpath -w "$SETUP")" \
  "-DESTIMATEDSIZEKB=$SIZE_KB" \
  "$(cygpath -w installer/kopiaui-wails.nsi)"

ls -la "$SETUP"
