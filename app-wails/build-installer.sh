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
#   VERSION     display version (default: <yyyymmdd>.0.0-<commit>); CI sets it
#               from the release tag
#   WINFSP_MSI  WinFsp installer to bundle (default: downloaded once to
#               ../dist/cache/ and checked against its SHA-256)
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
if [ -z "${VERSION:-}" ]; then
  COMMIT=$(git rev-parse --short=8 HEAD)
  if ! git diff --quiet HEAD -- . 2>/dev/null; then
    COMMIT="$COMMIT-dirty"
  fi
  VERSION="$DATE.0.0-$COMMIT"
fi
# Windows file versions are four numbers of at most 65535 each. Release tags
# like 0.23.1-wails.2 (upstream base version, fork release) map to 0.23.1.2.
if [[ "$VERSION" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)-wails\.([0-9]+)$ ]]; then
  VIVERSION="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${BASH_REMATCH[3]}.${BASH_REMATCH[4]}"
else
  VIVERSION="${DATE:0:4}.$((10#${DATE:4:4})).0.0"
fi

# WinFsp lets kopia mount snapshots as native drives instead of WebDAV ones.
# The installer carries its MSI and offers to install it. WinFsp is GPLv3
# with an exception for free software; the MSI is shipped unmodified.
WINFSP_VERSION=2.1.25156
WINFSP_URL="https://github.com/winfsp/winfsp/releases/download/v2.1/winfsp-$WINFSP_VERSION.msi"
WINFSP_SHA256=073a70e00f77423e34bed98b86e600def93393ba5822204fac57a29324db9f7a
WINFSP_MSI=${WINFSP_MSI:-$DIST/cache/winfsp-$WINFSP_VERSION.msi}

if [ ! -f "$WINFSP_MSI" ]; then
  mkdir -p "$(dirname "$WINFSP_MSI")"
  curl -fsSL -o "$WINFSP_MSI.tmp" "$WINFSP_URL"
  mv "$WINFSP_MSI.tmp" "$WINFSP_MSI"
fi

if ! echo "$WINFSP_SHA256  $WINFSP_MSI" | sha256sum -c --status; then
  echo "$WINFSP_MSI does not match the expected SHA-256" >&2
  exit 1
fi

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
  "-DWINFSPMSI=$(cygpath -w "$WINFSP_MSI")" \
  "-DWINFSPVERSION=$WINFSP_VERSION" \
  "$(cygpath -w installer/kopiaui-wails.nsi)"

ls -la "$SETUP"
