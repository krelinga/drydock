# Shared by stress.sh and selftest.sh. Expects WORK, FAILS and OUT to be set.

slug() { echo "$1" | tr -c 'A-Za-z0-9\n' '_'; }

# The file a failure's output is kept in: <slug pkg>__<slug test>.txt, with the
# shuffle seed of the iteration beside it in .seed.
outfile() { # pkg test
  if [ "$2" = "(package)" ]; then echo "$OUT/$(slug "$1")__package.txt"; else echo "$OUT/$(slug "$1")__$(slug "$2").txt"; fi
}

# Record the failing top-level tests of one `go test -json` output into FAILS
# and their output into OUT. Returns 2 for a build failure (a setup error, not
# a flake). A test still running when the package timed out has no fail event
# of its own, so it is named from its missing end; the output kept for it
# starts at the panic header that names it. A failure that names no test at all
# is one "(package)" entry.
record() { # pkg jsonfile [discard-timeouts]
  local pkg=$1 f=$2 discard=${3:-} tests hung seed t of
  if jq -e 'select(.Action=="build-fail" or (.FailedBuild != null))' "$f" >/dev/null; then
    return 2
  fi
  tests=$(jq -r 'select(.Action=="fail" and .Test != null and (.Test|contains("/")|not)) | .Test' "$f" | sort -u)
  hung=
  if [ -z "$tests" ] && jq -e 'select(.Action=="fail" and .Test == null)' "$f" >/dev/null; then
    # Started, never finished: the tests a timeout or a crash cut off.
    hung=$(jq -rs '[.[] | select(.Test != null and (.Test|contains("/")|not))]
      | (map(select(.Action=="run") | .Test) | unique) - (map(select(.Action=="pass" or .Action=="fail" or .Action=="skip") | .Test) | unique) | .[]' "$f")
    if [ -n "$discard" ] && grep -q 'panic: test timed out' "$f"; then
      return 0 # the timeout was ours, shortened to fit the deadline: not the test's fault
    fi
    tests=$hung
    [ -n "$tests" ] || tests="(package)"
  fi
  [ -n "$tests" ] || return 0
  seed=$(jq -r 'select(.Action=="output") | .Output' "$f" | sed -n 's/^-test\.shuffle \([0-9]*\)$/\1/p' | head -n1)
  while read -r t; do
    printf '%s\t%s\n' "$pkg" "$t" >>"$FAILS"
    of=$(outfile "$pkg" "$t")
    if [ "$t" = "(package)" ]; then
      jq -r 'select(.Action=="output") | .Output' "$f" | tail -n 80 >"$of"
    elif [ -n "$hung" ]; then
      jq -r --arg t "$t" 'select(.Action=="output" and .Test==$t) | .Output' "$f" | head -n 60 >"$of"
    else
      jq -r --arg t "$t" 'select(.Action=="output" and ((.Test // "")==$t or ((.Test // "")|startswith($t+"/")))) | .Output' "$f" | tail -n 80 >"$of"
    fi
    echo "${seed:-}" >"$of.seed"
  done <<<"$tests"
}
