#!/usr/bin/env bash
# The nightly stress loop (see .github/workflows/stress.yml). It only records:
# what failed goes to $STRESS_DIR as it happens, and file-issues.sh, which the
# workflow runs under `if: always()`, turns that into issues, so a job that is
# cancelled or times out still reports what it found.
#
# Environment: PACKAGES, MINUTES, CONTAINER_RUNS, FAKE_FAILURE, JOB_START (the
# job's start, epoch seconds, before checkout and tool setup), STRESS_DIR.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)

now() { date +%s; }
JOB_START=${JOB_START:-$(now)}
# The job has 60 minutes from its start. Tests stop for good at 52, leaving
# the issue-filing step room before the limit.
HARD_END=$((JOB_START + 52 * 60))

MINUTES=${MINUTES:-40}
case $MINUTES in '' | *[!0-9]*) MINUTES=40 ;; esac
[ "$MINUTES" -ge 1 ] || MINUTES=1
[ "$MINUTES" -le 40 ] || MINUTES=40
CONTAINER_RUNS=${CONTAINER_RUNS:-2}
case $CONTAINER_RUNS in '' | *[!0-9]*) CONTAINER_RUNS=2 ;; esac
[ "$CONTAINER_RUNS" -le 3 ] || CONTAINER_RUNS=3

ITER_SECS=480 # -timeout 8m
CONTAINER_SECS=1200
CONTAINER_MIN_EACH=7

WORK=${STRESS_DIR:?}
FAILS=$WORK/fails.tsv # package <TAB> test, one line per failure
RUNS=$WORK/runs.tsv   # package <TAB> 1, one line per iteration
OUT=$WORK/out
mkdir -p "$OUT"
: >"$FAILS"
: >"$RUNS"
echo "$MINUTES" >"$WORK/minutes"
# shellcheck source=lib.sh
. "$here/lib.sh"

PRIORITY="preview supervisor provision identity login events life catalog server"

remaining() { echo $((HARD_END - $(now))); }

# record, with a build failure turned into a failed job: that is a setup
# problem, not a flake.
record_or_die() { # pkg jsonfile [discard]
  local rc=0
  record "$@" || rc=$?
  if [ "$rc" = 2 ]; then
    echo "::error::build failed for $1"
    exit 1
  fi
  if [ "$rc" = 3 ]; then
    # Not silent: a real deadlock at the end of the budget looks the same.
    echo "::warning::$1 timed out at the limit shortened to fit the deadline; not filed"
    echo "$1 hit its shortened timeout and was not counted as a failure; check the log if that package hangs." >>"$WORK/notes"
    return 0
  fi
  return "$rc"
}

# Compile everything once, under -race, so a build error fails the job here
# instead of being mistaken for a flake, and the loop starts with a warm cache.
echo "::group::precompile"
go test -race -count=1 -run '^$' ./... >/dev/null
echo "::endgroup::"

PRIO=()
REST=()
if [ -n "${PACKAGES:-}" ]; then
  # shellcheck disable=SC2206
  PRIO=($PACKAGES)
else
  for p in $PRIORITY; do PRIO+=("./internal/$p"); done
  while read -r pkg; do
    short=${pkg#*/drydock/}
    case " $PRIORITY " in *" ${short#internal/} "*) continue ;; esac
    REST+=("./$short")
  done < <(go list ./internal/...)
fi

START=$(now)
LOOP_END=$((START + MINUTES * 60))
CONTAINER_NOTE="not run (container_runs=0)"
if [ "$CONTAINER_RUNS" -gt 0 ]; then
  need=$((CONTAINER_RUNS * CONTAINER_MIN_EACH * 60))
  if [ $((LOOP_END - START)) -ge $((need + 15 * 60)) ]; then
    LOOP_END=$((LOOP_END - need)) # the unit loop stops where the container tier starts
    CONTAINER_NOTE="$CONTAINER_RUNS runs"
  else
    CONTAINER_NOTE="skipped: ${MINUTES} min leaves no room for $CONTAINER_RUNS runs of ~${CONTAINER_MIN_EACH} min after the unit loop"
  fi
fi
[ "$LOOP_END" -le "$HARD_END" ] || LOOP_END=$HARD_END
echo "$CONTAINER_NOTE" >"$WORK/container-note"

stress_group() { # end-epoch pkg...
  local end=$1
  shift
  local left=$# pkg t slice pend f
  for pkg in "$@"; do
    t=$(now)
    if [ "$t" -ge "$end" ]; then break; fi
    slice=$(((end - t) / left))
    left=$((left - 1))
    pend=$((t + slice))
    # At least one iteration, then more until this package's slice is spent.
    while :; do
      # An iteration may run its whole timeout; never start one that could
      # pass the hard deadline.
      if [ "$(remaining)" -lt "$ITER_SECS" ]; then
        echo "hard deadline near: no more iterations"
        return 0
      fi
      f=$WORK/$(slug "$pkg").json
      go test -race -count=1 -shuffle=on -timeout "${ITER_SECS}s" -json "$pkg" >"$f" 2>"$f.err" || true
      [ -s "$f" ] || {
        cat "$f.err"
        echo "::error::no test output for $pkg"
        exit 1
      }
      record_or_die "$pkg" "$f"
      printf '%s\t1\n' "$pkg" >>"$RUNS"
      [ "$(now)" -lt "$pend" ] || break
    done
    echo "$pkg: $(awk -F'\t' -v p="$pkg" '$1==p {s+=$2} END {print s+0}' "$RUNS") iterations"
  done
}

# 75% of the unit loop for the priority packages, the rest for everything else.
PRIO_END=$((START + (LOOP_END - START) * 3 / 4))
[ ${#REST[@]} -gt 0 ] || PRIO_END=$LOOP_END
[ ${#PRIO[@]} -gt 0 ] && stress_group "$PRIO_END" "${PRIO[@]}"
[ ${#REST[@]} -gt 0 ] && stress_group "$LOOP_END" "${REST[@]}"

if [ "$CONTAINER_NOTE" = "$CONTAINER_RUNS runs" ]; then
  done_runs=0
  for _ in $(seq "$CONTAINER_RUNS"); do
    rem=$(remaining)
    if [ "$rem" -lt 600 ]; then
      CONTAINER_NOTE="$done_runs of $CONTAINER_RUNS runs: out of time"
      echo "$CONTAINER_NOTE" >"$WORK/container-note"
      break
    fi
    secs=$CONTAINER_SECS
    discard=
    if [ "$rem" -lt "$secs" ]; then
      secs=$rem
      discard=1 # a timeout at this shortened limit is not the tests' doing
    fi
    f=$WORK/container.json
    go test -count=1 -timeout "${secs}s" -json ./test/container >"$f" 2>"$f.err" || true
    [ -s "$f" ] || {
      cat "$f.err"
      echo "::error::no test output for test/container"
      exit 1
    }
    record_or_die ./test/container "$f" "$discard"
    printf './test/container\t1\n' >>"$RUNS"
    done_runs=$((done_runs + 1))
  done
fi

if [ -n "${FAKE_FAILURE:-}" ]; then
  printf './internal/sys\tTestInventedFlake\n' >>"$FAILS"
  echo "invented failure output, to prove the issue is filed" >"$(outfile ./internal/sys TestInventedFlake)"
  echo 1234567890 >"$(outfile ./internal/sys TestInventedFlake).seed"
  touch "$WORK/fake"
fi
echo "took $((($(now) - START) / 60)) min of a ${MINUTES} min loop" >"$WORK/took"
