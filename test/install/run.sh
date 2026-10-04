#!/usr/bin/env bash
# The installer test: deploy/install.sh against a real systemd, a real Caddy
# from its official package, and real TLS, in a throwaway privileged container
# (needs a Docker that allows --privileged: the devcontainer's DinD, or a
# GitHub-hosted runner). Every scenario is in
# scenario.sh; this only builds the releases it installs.
#
#   test/install/run.sh            # ~1 minute after the first image build
#   KEEP=1 test/install/run.sh     # leave the container up to poke at
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
name="drydock-install-test-$$"
# shellcheck source=test/install/lib.sh
. "$here/lib.sh"
cleanup() {
	stop_server "$name"
	rm -rf "$work"
}
trap cleanup EXIT

arch=$(go env GOARCH)
# Two real releases, for an upgrade, and a third whose binary cannot start,
# for the rollback.
"$root/deploy/package.sh" v0.0.1 "$work/releases/v0.0.1" "$arch" >/dev/null
"$root/deploy/package.sh" v0.0.2 "$work/releases/v0.0.2" "$arch" >/dev/null
"$root/deploy/package.sh" v0.0.3 "$work/releases/v0.0.3" "$arch" >/dev/null
broken="$work/broken/drydock"
mkdir -p "$broken"
tar -xzf "$work/releases/v0.0.3/drydock_linux_$arch.tar.gz" -C "$work/broken"
printf '#!/bin/sh\n[ "$1" = version ] && { echo v0.0.3; exit 0; }\necho "broken on purpose" >&2\nexit 1\n' >"$broken/drydock"
chmod +x "$broken/drydock"
tar -C "$work/broken" -czf "$work/releases/v0.0.3/drydock_linux_$arch.tar.gz" drydock
(cd "$work/releases/v0.0.3" && sha256sum drydock_linux_*.tar.gz >SHA256SUMS)
cp "$here/scenario.sh" "$work/"

start_server "$name"
docker cp "$work/releases" "$name:/releases"
docker cp "$work/scenario.sh" "$name:/scenario.sh"
docker exec "$name" bash /scenario.sh
