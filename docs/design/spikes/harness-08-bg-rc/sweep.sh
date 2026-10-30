#!/usr/bin/env bash
# Spike 08 — stop and remove every container this harness started, found by
# the harness's own label (dd-spike-08=1, which lib.sh's ctr_up sets on each),
# whatever run left it. A run killed before its EXIT trap reaped (SIGKILL, a
# tool timeout, a closed terminal) leaves its containers running, and a daemon
# or an orphaned worker in one holds the copied refresh token and refreshes it
# on its own schedule. run.sh and setup.sh run this before the token guard, so
# a refusal never leaves them running; run it by hand after an interrupted run,
# before anything else and before `rm -rf "$S"` (a daemon holds the token in
# memory, so deleting the file stops nothing).
#
# `docker stop -t 10` SIGTERMs PID 1 (the sleep loop, which exits at once, and
# the kernel then SIGKILLs every other process in the container), with SIGKILL
# after 10 s if it has not; then `docker rm`. Nothing without the label is
# touched. No claude is run, so a job's Remote Control session is left
# unarchived (spike result 9).
set -uo pipefail
LABEL=dd-spike-08=1
ids=$(docker ps -aq --filter "label=$LABEL") || { echo "sweep: docker ps failed; refusing to go on" >&2; exit 4; }
[ -n "$ids" ] || exit 0
# shellcheck disable=SC2086 # $ids is a whitespace-separated list of container ids
{
  echo "sweep: stopping and removing $(echo $ids | wc -w) leftover container(s) labelled $LABEL" >&2
  docker stop -t 10 $ids >/dev/null
  docker rm $ids >/dev/null
}
left=$(docker ps -aq --filter "label=$LABEL")
[ -z "$left" ] || { echo "sweep: still present: $left; refusing to go on" >&2; exit 4; }
exit 0
