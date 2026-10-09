#!/usr/bin/env bash
# Installs the pinned Caddy into /usr/local/bin, its tarball checked against
# the release's checksums. The one Caddy pin in CI: go-suite runs this, and so
# does the browser job, inside the Playwright image (where it is root and has
# no sudo). The devcontainer's Caddy feature takes the latest, so bump this
# when a rebuild moves it. test/install/lib.sh reads CADDY_VERSION from here for
# the installer test image's .deb (Caddy's cloudsmith apt repo answers 402).
set -euo pipefail

CADDY_VERSION=2.11.7

sudo=sudo
[ "$(id -u)" = 0 ] && sudo=

dir=$(mktemp -d)
base=https://github.com/caddyserver/caddy/releases/download/v$CADDY_VERSION
tarball=caddy_${CADDY_VERSION}_linux_amd64.tar.gz
(cd "$dir" &&
  curl -fsSLO "$base/$tarball" -fsSLO "$base/caddy_${CADDY_VERSION}_checksums.txt" &&
  grep " $tarball\$" "caddy_${CADDY_VERSION}_checksums.txt" | sha512sum -c -)
$sudo tar -xzf "$dir/$tarball" -C /usr/local/bin caddy
rm -rf "$dir"
caddy version
