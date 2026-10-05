#!/usr/bin/env bash
# The README's one-liner against a release's actual assets: the stamped
# install.sh, the tarball, the checksum. run.sh tests the installer; this tests
# what was actually uploaded.
#
#   test/install/live.sh v0.1.0             # a published release, from GitHub
#   test/install/live.sh latest             # what the README line installs today
#   test/install/live.sh --dir DIR v0.2.0   # the assets in DIR, served locally
#
# --dir is how the release workflow checks a release before anyone can see it:
# it downloads the *draft* release's assets (gh release download) and serves
# them from a local web server the way GitHub serves a published release, with
# the installer's DRYDOCK_DOWNLOAD_BASE pointed at it — the same route run.sh
# uses. The bytes installed are the bytes uploaded; only the host differs.
set -euo pipefail
usage() { echo "usage: $0 [--dir DIR] vX.Y.Z|latest" >&2; exit 2; }
dir=""
if [ "${1:-}" = --dir ]; then
	[ $# = 3 ] || usage
	dir=$(cd "$2" && pwd)
	shift 2
	[ "$1" != latest ] || { echo "--dir needs the release's tag, not latest" >&2; exit 2; }
fi
[ $# = 1 ] || usage
here=$(cd "$(dirname "$0")" && pwd)
name="drydock-install-live-$$"
# shellcheck source=test/install/lib.sh
. "$here/lib.sh"
trap 'stop_server "$name"' EXIT

start_server "$name"
docker cp "$here/live-scenario.sh" "$name:/live-scenario.sh"
assets=""
if [ -n "$dir" ]; then
	docker exec "$name" mkdir -p /assets
	docker cp "$dir/." "$name:/assets/$1"
	assets=/assets
fi
docker exec -e RELEASE="$1" -e ASSETS="$assets" -e REPO="${GITHUB_REPOSITORY:-krelinga/drydock}" \
	"$name" bash /live-scenario.sh
