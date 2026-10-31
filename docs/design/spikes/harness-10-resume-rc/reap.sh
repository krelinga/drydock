#!/usr/bin/env bash
# Spike 10 — stop everything a run left behind: SIGTERM each pane's process on
# this harness's private tmux server (dd-spike-10; the server pane last), then
# that tmux server, then every process whose environment still carries
# CLAUDE_CONFIG_DIR=$CFG (lib.sh's sweep: SIGTERM, then SIGKILL after 20 s,
# each pid printed). Nothing else is signalled. Run it before `rm -rf $S`:
# a process left holding the copied token could still refresh it.
#
# Usage: S=<scratch> ./reap.sh      exits non-zero if anything survives
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${S:?set S to the scratch dir setup.sh built}"
. "$HERE/lib.sh"
rc=0
reap || rc=1
t ls >/dev/null 2>&1 && { echo "tmux server dd-spike-10 still up"; rc=1; }
[ "$rc" -eq 0 ] && echo "reaped" || echo "NOT fully reaped"
exit "$rc"
