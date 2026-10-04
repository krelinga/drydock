#!/usr/bin/env bash
# The README's one-liner against a PUBLISHED release, downloaded from GitHub
# exactly as an operator would: the stamped install.sh, the tarball, the
# checksum. run.sh tests the installer; this tests what was actually uploaded.
# The release workflow runs it after every upload.
#
#   test/install/live.sh v0.1.0      # a specific release
#   test/install/live.sh latest      # what the README line installs today
set -euo pipefail
[ $# = 1 ] || { echo "usage: $0 vX.Y.Z|latest" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
name="drydock-install-live-$$"
# shellcheck source=test/install/lib.sh
. "$here/lib.sh"
trap 'stop_server "$name"' EXIT

start_server "$name"
docker cp "$here/live-scenario.sh" "$name:/live-scenario.sh"
docker exec -e RELEASE="$1" -e REPO="${GITHUB_REPOSITORY:-krelinga/drydock}" "$name" bash /live-scenario.sh
