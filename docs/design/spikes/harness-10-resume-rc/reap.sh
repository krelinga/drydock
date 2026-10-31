#!/usr/bin/env bash
# Spike 10 — stop everything a killed run left behind: SIGTERM each pane's
# process on this harness's private tmux server (dd-spike-10; the server pane
# last), then that tmux server. Spike 06's `reap`; nothing outside the private
# server is signalled.
#
# Usage: S=<scratch> ./reap.sh
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${S:?set S to the scratch dir setup.sh built}"
. "$HERE/lib.sh"
reap
t ls 2>/dev/null && { echo "tmux server dd-spike-10 still up"; exit 1; }
echo "reaped"
