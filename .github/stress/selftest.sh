#!/usr/bin/env bash
# Checks lib.sh's record() against recorded `go test -json` output (Go 1.27):
# a subtest's failure text, a timeout naming the hung test, a build failure.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
FAILS=$WORK/fails.tsv
OUT=$WORK/out
mkdir -p "$OUT"
: >"$FAILS"
# shellcheck source=lib.sh
. "$here/lib.sh"

fail() { echo "selftest: $*" >&2; exit 1; }

record ./sub "$here/testdata/subtest-fail.json"
[ "$(cat "$FAILS")" = "$(printf './sub\tTestParent')" ] || fail "subtest: wrong failures: $(cat "$FAILS")"
grep -q 'boom detail' "$(outfile ./sub TestParent)" || fail "subtest output lost its failure text"
grep -Eq '^[0-9]+$' "$(outfile ./sub TestParent).seed" || fail "shuffle seed lost"

: >"$FAILS"
record ./hang "$here/testdata/timeout.json"
[ "$(cat "$FAILS")" = "$(printf './hang\tTestHung')" ] || fail "timeout: hung test not named: $(cat "$FAILS")"
grep -q 'panic: test timed out' "$(outfile ./hang TestHung)" || fail "timeout header lost"
grep -q 'TestHung (2s)' "$(outfile ./hang TestHung)" || fail "running tests: list lost"

: >"$FAILS"
record ./hang "$here/testdata/timeout.json" discard
[ ! -s "$FAILS" ] || fail "a shortened timeout was recorded"

rc=0
record ./bad "$here/testdata/build-fail.json" || rc=$?
[ "$rc" = 2 ] || fail "build failure not detected (rc=$rc)"
rc=0
record ./sub "$here/testdata/subtest-fail.json" || rc=$?
[ "$rc" = 0 ] || fail "false build failure"
echo "selftest: ok"
