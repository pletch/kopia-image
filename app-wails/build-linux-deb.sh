#!/usr/bin/env bash
# Builds KopiaUI-Wails for Linux and packages it as a .deb.
#
# Usage (in app-wails/):
#   ./build-linux-deb.sh
#
# Needs Go, a C compiler and the GTK3 / WebKitGTK 4.1 headers
# (Debian/Ubuntu: apt install build-essential libgtk-3-dev libwebkit2gtk-4.1-dev).
# The binary links against the system GTK, so build on the oldest distribution
# the package should support; CI builds on Ubuntu 22.04 (Linux Mint 21).
#
# Environment:
#   VERSION  package version (default: <yyyymmdd>.0.0-<commit>); CI sets it from
#            the release tag. The package depends on kopia-wails of the same
#            version, which provides /usr/bin/kopia.
#
# Output: ../dist/kopiaui-wails_<version>_<arch>.deb
set -euo pipefail

cd "$(dirname "$0")"
umask 022

DIST=../dist
PKG=kopiaui-wails
APP_ID=io.kopia.KopiaUIWails # must match linuxApplicationID in main.go
ARCH=$(dpkg --print-architecture)

if [ -z "${VERSION:-}" ]; then
  COMMIT=$(git rev-parse --short=8 HEAD)
  if ! git diff --quiet HEAD -- . 2>/dev/null; then
    COMMIT="$COMMIT-dirty"
  fi
  VERSION="$(date +%Y%m%d).0.0-$COMMIT"
fi

echo "Building KopiaUI-Wails $VERSION for linux/$ARCH"

ROOT="$DIST/deb/${PKG}_${VERSION}_${ARCH}"
rm -rf "$ROOT"

CGO_ENABLED=1 go build -tags gtk3 -trimpath \
  -ldflags "-s -w -X main.buildVersion=$VERSION" \
  -o "$ROOT/usr/bin/kopiaui-wails" .

install -Dm644 icons/app.png "$ROOT/usr/share/icons/hicolor/256x256/apps/$APP_ID.png"
install -Dm644 ../LICENSE "$ROOT/usr/share/doc/$PKG/copyright"

install -d "$ROOT/usr/share/applications"
cat > "$ROOT/usr/share/applications/$APP_ID.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=KopiaUI-Wails
GenericName=Backup
Comment=Back up and restore files with Kopia
Exec=kopiaui-wails
Icon=$APP_ID
Terminal=false
Categories=Utility;Archiving;
Keywords=backup;restore;snapshot;kopia;
StartupWMClass=$APP_ID
EOF

install -d "$ROOT/DEBIAN"
cat > "$ROOT/DEBIAN/control" <<EOF
Package: $PKG
Version: $VERSION
Architecture: $ARCH
Maintainer: ${GITHUB_REPOSITORY_OWNER:-kopia-wailsUI} <${GITHUB_REPOSITORY_OWNER:-kopia-wailsUI}@users.noreply.github.com>
Installed-Size: $(du -sk "$ROOT/usr" | cut -f1)
Depends: kopia-wails (= $VERSION), libgtk-3-0t64 | libgtk-3-0, libwebkit2gtk-4.1-0
Recommends: fuse3
Section: utils
Priority: optional
Homepage: https://github.com/${GITHUB_REPOSITORY:-pletch/kopia-wailsUI}
Description: KopiaUI built with Wails (personal fork)
 Desktop shell for Kopia with a tray icon, notifications and repository
 windows, built from a personal fork of kopia/kopia. It runs the kopia
 server from the kopia-wails package.
EOF

DEB="$DIST/${PKG}_${VERSION}_${ARCH}.deb"
dpkg-deb --root-owner-group --build "$ROOT" "$DEB"
dpkg-deb --info "$DEB"
ls -la "$DEB"
