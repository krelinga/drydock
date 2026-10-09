#!/usr/bin/env bash
# The nightly stress loop (see .github/workflows/stress.yml). Inputs come from
# the environment: PACKAGES, MINUTES, CONTAINER_RUNS, FAKE_FAILURE, RUN_URL,
# COMMIT, and GH_TOKEN/GH_REPO for gh.
set -euo pipefail

MINUTES=${MINUTES:-40}
CONTAINER_RUNS=${CONTAINER_RUNS:-2}
ITER_TIMEOUT=8m
CONTAINER_MIN_EACH=7
WORK=$(mktemp -d)
FAILS=$WORK/fails.tsv # package <TAB> test, one line per failure
RUNS=$WORK/runs.tsv   # package <TAB> iterations
OUT=$WORK/out
mkdir -p "$OUT"
: >"$FAILS"
: >"$RUNS"

PRIORITY="preview supervisor provision identity login events life catalog server"

now() { date +%s; }
slug() { echo "$1" | tr -c 'A-Za-z0-9\n' '_'; }
summary() { echo "$@" | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"; }
outfile() { # pkg test
  if [ "$2" = "(package)" ]; then echo "$OUT/$(slug "$1")__package.txt"; else echo "$OUT/$(slug "$1")__$(slug "$2").txt"; fi
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
  if [ $((MINUTES * 60)) -ge $((need + 15 * 60)) ]; then
    LOOP_END=$((LOOP_END - need)) # the unit loop stops where the container tier starts
    CONTAINER_NOTE="$CONTAINER_RUNS runs"
  else
    CONTAINER_NOTE="skipped: ${MINUTES} min leaves no room for $CONTAINER_RUNS runs of ~${CONTAINER_MIN_EACH} min after the unit loop"
  fi
fi

# Record the failing top-level tests of one `go test -json` output. A failure
# with no test named (a package-level panic or timeout) is one "(package)"
# entry; a build failure is a setup error and fails the job.
record() { # pkg jsonfile
  local pkg=$1 f=$2 tests t
  if grep -q '\[build failed\]' "$f"; then
    echo "::error::build failed for $pkg"
    exit 1
  fi
  tests=$(jq -r 'select(.Action=="fail" and .Test != null and (.Test|contains("/")|not)) | .Test' "$f" | sort -u)
  if [ -z "$tests" ] && jq -e 'select(.Action=="fail" and .Test == null)' "$f" >/dev/null; then
    tests="(package)"
  fi
  [ -z "$tests" ] && return 0
  while read -r t; do
    printf '%s\t%s\n' "$pkg" "$t" >>"$FAILS"
    if [ "$t" = "(package)" ]; then
      jq -r 'select(.Action=="output") | .Output' "$f" | tail -n 60 >"$(outfile "$pkg" "$t")"
    else
      jq -r --arg t "$t" 'select(.Test==$t and .Action=="output") | .Output' "$f" | tail -n 60 >"$(outfile "$pkg" "$t")"
    fi
  done <<<"$tests"
}

stress_group() { # end-epoch pkg...
  local end=$1
  shift
  local left=$# pkg n t slice pend f
  for pkg in "$@"; do
    t=$(now)
    if [ "$t" -ge "$end" ]; then break; fi
    slice=$(((end - t) / left))
    left=$((left - 1))
    n=0
    pend=$((t + slice))
    # At least one iteration, then more until this package's slice is spent.
    while :; do
      f=$WORK/$(slug "$pkg").json
      go test -race -count=1 -shuffle=on -timeout "$ITER_TIMEOUT" -json "$pkg" >"$f" 2>"$f.err" || true
      [ -s "$f" ] || {
        cat "$f.err"
        echo "::error::no test output for $pkg"
        exit 1
      }
      record "$pkg" "$f"
      n=$((n + 1))
      [ "$(now)" -lt "$pend" ] || break
    done
    printf '%s\t%s\n' "$pkg" "$n" >>"$RUNS"
    echo "$pkg: $n iterations"
  done
}

# 75% of the unit loop for the priority packages, the rest for everything else.
PRIO_END=$((START + (LOOP_END - START) * 3 / 4))
[ ${#REST[@]} -gt 0 ] || PRIO_END=$LOOP_END
[ ${#PRIO[@]} -gt 0 ] && stress_group "$PRIO_END" "${PRIO[@]}"
[ ${#REST[@]} -gt 0 ] && stress_group "$LOOP_END" "${REST[@]}"

if [ "$CONTAINER_NOTE" = "$CONTAINER_RUNS runs" ]; then
  n=0
  for _ in $(seq "$CONTAINER_RUNS"); do
    f=$WORK/container.json
    go test -count=1 -timeout 20m -json ./test/container >"$f" 2>"$f.err" || true
    [ -s "$f" ] || {
      cat "$f.err"
      echo "::error::no test output for test/container"
      exit 1
    }
    record ./test/container "$f"
    n=$((n + 1))
  done
  printf '%s\t%s\n' ./test/container "$n" >>"$RUNS"
fi

TITLE_PREFIX=""
if [ -n "${FAKE_FAILURE:-}" ]; then
  printf './internal/sys\tTestInventedFlake\n' >>"$FAILS"
  echo "invented failure output, to prove the issue is filed" >"$(outfile ./internal/sys TestInventedFlake)"
  TITLE_PREFIX="[test] "
fi

summary "## Stress run"
summary "Loop budget ${MINUTES} min, took $(((($(now) - START)) / 60)) min. test/container: $CONTAINER_NOTE."
summary ""
summary "| package | iterations |"
summary "|---|---|"
while IFS=$'\t' read -r p n; do summary "| $p | $n |"; done <"$RUNS"

if [ ! -s "$FAILS" ]; then
  summary ""
  summary "No test failed."
  exit 0
fi

gh label create flake --color d93f0b --description "A test that failed in the nightly stress run" 2>/dev/null || true

sort "$FAILS" | uniq -c | while read -r k pkg test; do
  # `read` splits on spaces; the (package) entry has none, test names never do.
  iters=$(awk -F'\t' -v p="$pkg" '$1==p {s+=$2} END {print s+0}' "$RUNS")
  title="${TITLE_PREFIX}flake: ${pkg#./} $test"
  body=$WORK/body.md
  {
    echo "@krelinga: \`$test\` in \`${pkg#./}\` failed $k of $iters iterations in this stress run."
    if [ "$k" -ge "$iters" ]; then
      echo
      echo "It failed every time, so it may be broken rather than flaky."
    fi
    echo
    echo "- Run: $RUN_URL"
    echo "- Commit: $COMMIT"
    echo "- Command: \`go test -race -count=1 -shuffle=on -timeout $ITER_TIMEOUT $pkg\`"
    echo
    echo "Tail of the last failing output:"
    echo
    echo '```'
    tail -c 6000 "$(outfile "$pkg" "$test")" | sed 's/```/` ` `/g'
    echo '```'
  } >"$body"
  existing=$(gh issue list --label flake --state open --limit 200 --json number,title |
    jq -r --arg t "$title" '.[] | select(.title==$t) | .number' | head -n1)
  if [ -n "$existing" ]; then
    gh issue comment "$existing" --body-file "$body"
    summary "- commented on #$existing: $title ($k/$iters)"
  else
    url=$(gh issue create --title "$title" --label flake --assignee krelinga --body-file "$body")
    summary "- filed $url: $title ($k/$iters)"
  fi
done
