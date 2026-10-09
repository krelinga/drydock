#!/usr/bin/env bash
# Files what stress.sh recorded in $STRESS_DIR. The workflow runs this under
# `if: always()`, so a cancelled or timed-out loop still reports. Environment:
# STRESS_DIR, GH_TOKEN, GH_REPO, RUN_URL, COMMIT.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
WORK=${STRESS_DIR:?}
CAP=5 # more distinct failures than this in one run is one summary issue

# Nothing to do if setup failed before the loop began; that failure is the
# workflow's own.
[ -f "$WORK/fails.tsv" ] || exit 0
FAILS=$WORK/fails.tsv
RUNS=$WORK/runs.tsv
OUT=$WORK/out
# shellcheck source=lib.sh
. "$here/lib.sh"

summary() { echo "$@" | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"; }
note() { cat "$WORK/$1" 2>/dev/null || true; }

PREFIX=""
[ ! -f "$WORK/fake" ] || PREFIX="[test] "

summary "## Stress run"
summary "$(note took). test/container: $(note container-note)."
summary ""
summary "| package | iterations |"
summary "|---|---|"
awk -F'\t' '{n[$1]+=$2} END {for (p in n) printf "| %s | %d |\n", p, n[p]}' "$RUNS" | sort | while read -r line; do summary "$line"; done

if [ ! -s "$FAILS" ]; then
  summary ""
  summary "No test failed."
  exit 0
fi

gh label create flake --color d93f0b --description "A test that failed in the nightly stress run" 2>/dev/null || true

iters_of() { awk -F'\t' -v p="$1" '$1==p {s+=$2} END {print s+0}' "$RUNS"; }
repro() { # pkg seed
  local race=-race
  [ "$1" != ./test/container ] || race=
  echo "go test $race -count=1 -shuffle=${2:-on} $1"
}

sort "$FAILS" | uniq -c | awk '{c=$1; $1=""; sub(/^ /,""); print c "\t" $0}' >"$WORK/distinct.tsv"
distinct=$(wc -l <"$WORK/distinct.tsv")
body=$WORK/body.md

if [ "$distinct" -gt "$CAP" ]; then
  title="${PREFIX}flake: $distinct tests failed in the stress run of $(date -u +%F)"
  {
    echo "@krelinga: $distinct distinct tests failed in one stress run, which usually means something shared broke (a registry, the network, a runner) rather than $distinct flakes. They are listed here instead of filed one by one."
    echo
    echo "- Run: $RUN_URL"
    echo "- Commit: $COMMIT"
    echo
    echo "| package | test | failed | of |"
    echo "|---|---|---|---|"
    while IFS=$'\t' read -r k rest; do
      pkg=${rest%% *}
      test=${rest#* }
      i=$(iters_of "$pkg")
      [ "$i" -ge "$k" ] || i=$k
      echo "| \`${pkg#./}\` | \`$test\` | $k | $i |"
    done <"$WORK/distinct.tsv"
    echo
    echo "Output for the first failure:"
    echo
    echo '```'
    first=$(head -n1 "$WORK/distinct.tsv")
    first=${first#*$'\t'}
    tail -c 4000 "$(outfile "${first%% *}" "${first#* }")" | sed 's/```/` ` `/g'
    echo '```'
  } >"$body"
  url=$(gh issue create --title "$title" --label flake --assignee krelinga --body-file "$body")
  summary "- filed $url: $title"
  exit 0
fi

while IFS=$'\t' read -r k rest; do
  pkg=${rest%% *}
  test=${rest#* }
  iters=$(iters_of "$pkg")
  [ "$iters" -ge "$k" ] || iters=$k
  title="${PREFIX}flake: ${pkg#./} $test"
  of=$(outfile "$pkg" "$test")
  seed=$(cat "$of.seed" 2>/dev/null || true)
  {
    echo "@krelinga: \`$test\` in \`${pkg#./}\` failed $k of $iters iterations in this stress run."
    if [ "$k" -ge "$iters" ]; then
      echo
      echo "It failed every time, so it may be broken rather than flaky."
    fi
    echo
    echo "- Run: $RUN_URL"
    echo "- Commit: $COMMIT"
    echo "- Shuffle seed of the last failure: \`${seed:-unknown}\`"
    echo "- Reproduce the order: \`$(repro "$pkg" "$seed")\`"
    echo
    echo "Output of the last failure:"
    echo
    echo '```'
    tail -c 6000 "$of" | sed 's/```/` ` `/g'
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
done <"$WORK/distinct.tsv"
