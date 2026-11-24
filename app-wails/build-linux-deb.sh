#!/usr/bin/env bash
# Builds Kopia-Image for Linux and packages it, with its own kopia, as a .deb.
#
# Usage (in app-wails/):
#   ./build-linux-deb.sh
#
# Needs Go, a C compiler and the GTK3 / WebKitGTK 4.1 headers
# (Debian/Ubuntu: apt install build-essential libgtk-3-dev libwebkit2gtk-4.1-dev).
# The binary links against the system GTK, so build on the oldest distribution
# the package should support; CI builds on Ubuntu 22.04 (Linux Mint 21).
#
# Like upstream KopiaUI, the package carries a private kopia that the app runs as
# its server, so it does not depend on the kopia-wails (CLI) package and does not
# clash with it:
#   /usr/lib/kopia-image/kopia-image     the app
#   /usr/lib/kopia-image/server/kopia    its kopia (found next to the app)
#   /usr/bin/kopia-image                 symlink to the app
#
# It replaces kopiaui-wails, the package's name before the rename.
#
# Environment:
#   VERSION    package version (default: <yyyymmdd>.0.0-<commit>); CI sets it from
#              the release tag
#   KOPIA_BIN  kopia binary to bundle (default: built from this repository)
#
# Output: ../dist/kopia-image_<version>_<arch>.deb
set -euo pipefail

cd "$(dirname "$0")"
umask 022

DIST=../dist
PKG=kopia-image
LEGACY_PKG=kopiaui-wails
APP_ID=io.kopia.KopiaImage # must match linuxApplicationID in main.go
ARCH=$(dpkg --print-architecture)
LIBDIR=usr/lib/$PKG

if [ -z "${VERSION:-}" ]; then
  COMMIT=$(git rev-parse --short=8 HEAD)
  if ! git diff --quiet HEAD -- .. 2>/dev/null; then
    COMMIT="$COMMIT-dirty"
  fi
  VERSION="$(date +%Y%m%d).0.0-$COMMIT"
fi

echo "Building Kopia-Image $VERSION for linux/$ARCH"

ROOT="$DIST/deb/${PKG}_${VERSION}_${ARCH}"
rm -rf "$ROOT"

CGO_ENABLED=1 go build -tags gtk3 -trimpath \
  -ldflags "-s -w -X main.buildVersion=$VERSION" \
  -o "$ROOT/$LIBDIR/kopia-image" .

if [ -n "${KOPIA_BIN:-}" ]; then
  install -Dm755 "$KOPIA_BIN" "$ROOT/$LIBDIR/server/kopia"
else
  # same flags as the kopia CLI built by the release workflow
  (cd .. && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/kopia/kopia/repo.BuildVersion=$VERSION -X github.com/kopia/kopia/repo.BuildInfo=${GITHUB_SHA:-$(git rev-parse HEAD)} -X github.com/kopia/kopia/repo.BuildGitHubRepo=${GITHUB_REPOSITORY:-pletch/kopia-image}" \
    -o "app-wails/$ROOT/$LIBDIR/server/kopia" github.com/kopia/kopia)
fi

"$ROOT/$LIBDIR/server/kopia" --version

install -d "$ROOT/usr/bin"
ln -s "../lib/$PKG/kopia-image" "$ROOT/usr/bin/kopia-image"

install -Dm644 icons/app.png "$ROOT/usr/share/icons/hicolor/256x256/apps/$APP_ID.png"
install -Dm644 ../LICENSE "$ROOT/usr/share/doc/$PKG/copyright"

install -d "$ROOT/usr/share/applications"
cat > "$ROOT/usr/share/applications/$APP_ID.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=Kopia-Image
GenericName=Backup
Comment=Back up and restore files with Kopia
Exec=kopia-image
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
Maintainer: ${GITHUB_REPOSITORY_OWNER:-pletch} <${GITHUB_REPOSITORY_OWNER:-pletch}@users.noreply.github.com>
Installed-Size: $(du -sk "$ROOT/usr" | cut -f1)
Depends: libgtk-3-0t64 | libgtk-3-0, libwebkit2gtk-4.1-0
Recommends: fuse3
Replaces: $LEGACY_PKG
Conflicts: $LEGACY_PKG
Provides: $LEGACY_PKG
Section: utils
Priority: optional
Homepage: https://github.com/${GITHUB_REPOSITORY:-pletch/kopia-image}
Description: Kopia desktop app with disk images (unofficial fork)
 Desktop shell for Kopia with a tray icon, notifications and repository
 windows, built from a personal fork of kopia/kopia. It includes its own
 kopia, which it runs as its server; install kopia-wails as well to use
 kopia from a terminal.
EOF

DEB="$DIST/${PKG}_${VERSION}_${ARCH}.deb"
dpkg-deb --root-owner-group --build "$ROOT" "$DEB"
dpkg-deb --info "$DEB"
ls -la "$DEB"
