#!/usr/bin/env bash
# The installer test: deploy/install.sh against a real systemd, a real Caddy
# from its official package, and real TLS, in a throwaway privileged container
# (needs the devcontainer's docker-in-docker). Every scenario is in
# scenario.sh; this only builds the releases it installs.
#
#   test/install/run.sh            # ~1 minute after the first image build
#   KEEP=1 test/install/run.sh     # leave the container up to poke at
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
name="drydock-install-test-$$"
cleanup() {
	[ "${KEEP:-0}" = 1 ] && echo "container left running: docker exec -it $name bash" ||
		docker rm -f "$name" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

# The VS Code credential helper dies with its window; the base image is public.
if [ -z "${DOCKER_CONFIG:-}" ]; then
	export DOCKER_CONFIG="$work/docker"
	mkdir -p "$DOCKER_CONFIG" && echo '{}' >"$DOCKER_CONFIG/config.json"
fi
docker build -q -t drydock-install-test "$here" >/dev/null

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

docker run -d --name "$name" --privileged --cgroupns=host \
	-v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock \
	drydock-install-test >/dev/null
docker cp "$work/releases" "$name:/releases"
docker cp "$work/scenario.sh" "$name:/scenario.sh"
for _ in $(seq 1 50); do
	state=$(docker exec "$name" systemctl is-system-running 2>/dev/null || true)
	case "$state" in running | degraded) break ;; esac
	sleep 0.2
done
docker exec "$name" bash /scenario.sh
