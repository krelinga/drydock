#!/usr/bin/env bash
# Spike 09 — the split layout: a per-container CLAUDE_CONFIG_DIR and one
# credential dir shared through CLAUDE_SECURESTORAGE_CONFIG_DIR, with
# oauthAccount seeded into each container's .claude.json. See
# ../09-split-credential.md for the question and the results.
#
# Usage: S=<scratch> ./run.sh <mode>     (build <scratch> with ./setup.sh first)
#
#  Fake credential only (no real token is ever mounted; Spike 00's fake):
#   lock       results 1, 6: four containers on one shared credential dir with an
#              expired fake credential, the token host blackholed, all refreshing
#              at once (two `claude -p`, a `remote-control` server, a `--bg`
#              daemon): where .oauth_refresh.lock, its .owner record and the
#              legacy lock land, and who holds them. NOVAR=1 is the control: the
#              shared layout's way with per-container config dirs (no variable),
#              each holding its own copy
#   blank      result 1: the same credential dir, NOT blackholed: the fake
#              refresh token is refused (invalid_grant), the credential is blanked
#              — whether every container sees it. HOLD=1 holds the credential
#              write lock (<dir>/.storage-write.lock) first: if the blank then
#              does not land, that is where the write lock is
#   bump       result 7: the bump-ritual check. Asserts, with a positive control
#              for each, that under the variable the credential is read from and
#              the refresh lock taken in the variable's dir, and the account record
#              read from CLAUDE_CONFIG_DIR; and that today's shared layout, one dir
#              mounted at both paths, still keeps them together. Exits 1 on any failure.
#              BUMP_MUTATE= runs it against a deliberate break, to watch it fail:
#                var   the containers get the variable misspelled
#                      (CLAUDE_SECURESTORAGE_CONFIG_DIRX): the variable ignored
#                lock  a copy of the binary in $S, an equal-length byte patch of
#                      the refresh lock's name (".oauth_refresh.lock" ->
#                      "../.claude/o_r.lock"), so the refresh locks in the config
#                      dir and the credential stays in the variable's: the bump
#                      that splits the lock from the credential (fake only)
#
#  The owner's copied credential (token guard; every mode reaps what it starts):
#   seed       results 1, 2: three containers on one shared credential copy, own
#              configs: oauthAccount seeded / absent (the control) / given by
#              CLAUDE_CODE_{ACCOUNT_UUID,USER_EMAIL,ORGANIZATION_UUID}; does
#              `claude --bg --rc` get Remote Control; what rewrites the record
#   server     result 2: today's model under split: a `claude remote-control`
#              server in a seeded container and in an unseeded one (the control);
#              its environment id and Capacity, then a SIGTERM stop, then the same
#              server started again in each (does the unseeded one ever recover?)
#   two        result 3: two containers, own daemons, both at /workspace (DISTINCT=1:
#              /workspaces/ws-a, ws-b); listings, stop and respawn across them
#   attach     result 4: `claude attach` from a second exec; one turn typed there
#   supervise  result 5: Spike 08's supervision recap under split: a foreground
#              daemon, the link, stop, respawn, docker stop/start, daemon stop --any
#   app        human step H1: the phone sends, the laptop attaches and sends
#              (pauses WAIT_APP s twice)
#
#  Reading only (runs no claude):
#   read       result 6 (read): the variable's resolver and every use the
#              results rest on, printed from the bundled JavaScript
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODE="${1:?usage: S=<scratch> ./run.sh <mode>}"
case "$MODE" in
  lock|blank|bump|seed|server|two|attach|supervise|app|read) ;;
  *) printf 'unknown mode: %s\n' "$MODE"; exit 2 ;;
esac
: "${S:?set S to the scratch dir setup.sh built}"
if [ -n "${DISTINCT:-}" ] && [ "$MODE" != two ]; then printf 'DISTINCT=1 is for the two mode only\n'; exit 2; fi
if [ -n "${NOVAR:-}" ] && [ "$MODE" != lock ]; then printf 'NOVAR=1 is for the lock mode only\n'; exit 2; fi
if [ -n "${HOLD:-}" ] && [ "$MODE" != blank ]; then printf 'HOLD=1 is for the blank mode only\n'; exit 2; fi
case "${BUMP_MUTATE:-}" in
  '') ;;
  var|lock) [ "$MODE" = bump ] || { printf 'BUMP_MUTATE= is for the bump mode only\n'; exit 2; } ;;
  *) printf 'BUMP_MUTATE=%s? (var or lock)\n' "$BUMP_MUTATE"; exit 2 ;;
esac
[ "${BUMP_MUTATE:-}" = var ] && VARNAME=CLAUDE_SECURESTORAGE_CONFIG_DIRX
# First, before the guard can refuse: whatever a killed run left (sweep.sh).
"$HERE/sweep.sh" || exit 4
RUN="$S/run/$MODE"
FAKE="$S/fake"
case "$MODE" in lock|blank|bump) CRED="$FAKE/cred" ;; esac
. "$HERE/lib.sh"   # refuses dirs that are not under $S or are the real config
rm -rf "$RUN"; mkdir -p "$RUN"
W_APP="${WAIT_APP:-300}"
is_uint "$W_APP" || { say "REFUSING: WAIT_APP='$W_APP' is not a whole number of seconds (no leading zero, at most 15 digits)"; exit 2; }
case "$MODE" in
  seed|server|two|attach|supervise) token_guard "$CRED/.credentials.json" 10 ;;
  app) token_guard "$CRED/.credentials.json" $(( (2 * W_APP + 59) / 60 + 10 )) ;;
  lock|blank|bump) # the fake modes must never see the real copy
     [ "$(_rp "$CRED")" != "$(_rp "$S/cred")" ] || { say "REFUSING: a fake mode on the real cred dir"; exit 2; } ;;
  read) ;;
esac
# The copy's hash as the guard passed it; cred_check compares it at exit.
case "$MODE" in seed|server|two|attach|supervise|app) sha256sum < "$CRED/.credentials.json" > "$RUN/cred.sha256" ;; esac
trap 'reap; cred_check; rm -f "$S/claude-mutated"' EXIT
reap
if [ "${BUMP_MUTATE:-}" = lock ]; then
  # The refresh locks `join(HS(), ".oauth_refresh.lock")` (dkn). Swap that
  # name, in a copy in $S, for one of the same length that climbs out of the
  # variable's dir into CLAUDE_CONFIG_DIR, "../.claude/o_r.lock": the refresh
  # then locks in the config dir while the credential, its .owner record
  # (".oauth_refresh.lock.owner", a constant of its own) and everything else
  # stay where they were. Every copy of the bare name is patched: the bundled
  # source's and the bytecode's constant (the binary runs the bytecode; a
  # source-only patch was tried first and changed nothing).
  NAME='.oauth_refresh.lock'; PATCH='../.claude/o_r.lock'
  M="$S/claude-mutated"; not_real "$M"
  cp "$CLAUDE" "$M" && chmod 755 "$M" || exit 1
  n=0
  for off in $(grep -aboF "$NAME" "$M" | cut -d: -f1); do
    [ "$(dd if="$M" bs=1 skip=$(( off + ${#NAME} )) count=1 status=none | tr -d '\000')" = . ] && continue  # .oauth_refresh.lock.owner
    printf '%s' "$PATCH" | dd of="$M" bs=1 seek="$off" conv=notrunc status=none || exit 1
    n=$(( n + 1 )); say "BUMP_MUTATE=lock: '$NAME' -> '$PATCH' at byte $off"
  done
  [ "$n" -ge 1 ] || { say "BUMP_MUTATE=lock: no bare '$NAME' in this binary; this mutation is not available on $("$CLAUDE" --version 2>&1 | head -1)"; exit 2; }
  CLAUDE="$M"
fi
[ "${BUMP_MUTATE:-}" = var ] && say "BUMP_MUTATE=var: every container gets $VARNAME instead of CLAUDE_SECURESTORAGE_CONFIG_DIR"
say "claude under test: $("$CLAUDE" --version 2>&1 | head -1) ($CLAUDE)"

# fake_setup: the fake modes' state, rebuilt every run: an empty shared cred
# dir and a fake account record (no personal values anywhere).
fake_setup() {
  not_real "$FAKE"; rm -rf "$FAKE"; mkdir -p "$FAKE/cred"; chmod 700 "$FAKE/cred"
  printf '{"accountUuid":"00000000-0000-4000-8000-00000000000a","emailAddress":"spike09@example.invalid","organizationUuid":"00000000-0000-4000-8000-0000000000f0"}\n' > "$FAKE/acct.json"
}
# legacy_watch <w> <path>: inside container <w>, sample every 50 s/1000 whether
# <path> (the legacy refresh lock, beside the credential dir) exists, for 60 s,
# into the workspace's .legacy file.
legacy_watch() {
  docker exec -d -u vscode "$(ctr "$1")" sh -c "for i in \$(seq 1 1200); do [ -e '$2' ] && echo held; sleep 0.05; done > '$(wsdir "$1")/.legacy'"
}

case "$MODE" in
# ---------------------------------------------------------------------------
lock)
  fake_setup
  fake_cred "$FAKE/cred/.credentials.json" expired
  dirs=("$FAKE/cred")
  for w in a b c d; do
    if [ -n "${NOVAR:-}" ]; then
      ACCT=$FAKE/acct.json mkcfg "l$w" seeded "$FAKE/cred/.credentials.json"
      ctr_up "$w" "$(cfgdir "l$w")" - "${BLACKHOLE[@]}" || exit 1
    else
      ACCT=$FAKE/acct.json mkcfg "l$w" seeded
      ctr_up "$w" "$(cfgdir "l$w")" "$FAKE/cred" "${BLACKHOLE[@]}" || exit 1
    fi
    dirs+=("$(cfgdir "l$w")")
  done
  [ -n "${NOVAR:-}" ] && { rm -f "$FAKE/cred/.credentials.json"; say "NOVAR=1: no variable; each config holds its own copy of the expired fake credential"; }
  LEG=$IN_CRED.lock; [ -n "${NOVAR:-}" ] && LEG=$IN_CFG.lock
  for w in a b c d; do rm -f "$S/ws-$w/.dbg" "$S/ws-$w/.legacy"; legacy_watch "$w" "$LEG"; done
  watch_locks 55 "$RUN/watch.txt" "${dirs[@]}" &
  WP=$!
  sleep 1
  say "--- at once: a, b: claude -p hi; c: claude remote-control; d: claude --bg --remote-control"
  XT=50 C a -p hi --debug-file "$(wsdir a)/.dbg" > "$RUN/a.out" &
  XT=50 C b -p hi --debug-file "$(wsdir b)/.dbg" > "$RUN/b.out" &
  pane rc "$RUN/c.pane" docker exec -it -u vscode -w "$(wsdir c)" "$(ctr c)" claude remote-control --name ddspike09lock --debug-file "$(wsdir c)/.dbg"
  ID=$(bg d --remote-control ddspike09lockd --debug-file "$(wsdir d)/.dbg"); say "  d: job ${ID:-none} (exit $(cat "$RUN/bg.rc"))"
  wait "$WP"
  say "--- the lock entries over 55 s (host side; .owner mapped to its container by pid namespace):"
  sed 's/^/    /' "$RUN/watch.txt"
  for w in a b c d; do
    say "  $w: legacy lock $LEG held in $(awk '/held/ {n++} END {print n+0}' "$S/ws-$w/.legacy" 2>/dev/null) of 1200 samples, 50 ms apart"
    say "  $w: debug lines about the refresh lock:"
    grep -aiE 'oauth.*(lock|refresh)|refresh.*lock|ELOCKED|another process' "$S/ws-$w/.dbg" 2>/dev/null | cut -c1-170 | head -6 | sed 's/^/      > /'
  done
  say "  c's server screen tail:"; screen rc | grep -v '^\s*$' | tail -4 | cut -c1-160 | sed 's/^/    | /'
  [ -n "$ID" ] && say "  d's daemon.log, refresh lines:" && grep -aE 'auth:' "$(cfgdir ld)/daemon.log" 2>/dev/null | cut -c1-170 | head -6 | sed 's/^/    > /'
  say "  the shared cred dir: $(tree "$FAKE/cred")"
  for w in a b c d; do say "  cfg-l$w: $(tree "$(cfgdir "l$w")" | tr ';' '\n' | grep -E 'credential|lock|storage|\.claude\.json$' | tr '\n' ';')"; done
  ;;
# ---------------------------------------------------------------------------
blank)
  fake_setup
  fake_cred "$FAKE/cred/.credentials.json" expired
  for w in a b; do ACCT=$FAKE/acct.json mkcfg "k$w" seeded; ctr_up "$w" "$(cfgdir "k$w")" "$FAKE/cred" || exit 1; rm -f "$S/ws-$w/.dbg"; done
  say "  before: cred $(cred_shape "$FAKE/cred/.credentials.json")"
  if [ -n "${HOLD:-}" ]; then
    # Hold the credential write lock where the reading puts it (proper-lockfile's
    # <file>.lock for <HS()>/.storage-write), fresh (touched every 2 s, under its
    # 15 s staleness), so a write that takes it there must wait and give up.
    mkdir "$FAKE/cred/.storage-write.lock"
    ( for i in $(seq 1 25); do [ -d "$FAKE/cred/.storage-write.lock" ] || exit 0; touch -c "$FAKE/cred/.storage-write.lock"; sleep 2; done ) &
    TOUCHER=$!
    say "HOLD=1: holding $IN_CRED/.storage-write.lock for the run"
  fi
  WATCH_SLEEP=0 watch_locks 40 "$RUN/watch.txt" "$FAKE/cred" "$(cfgdir ka)" "$(cfgdir kb)" &
  WP=$!; sleep 1
  say "--- a: claude -p hi with the expired fake (the token endpoint is real: it refuses the fake refresh token)"
  XT=35 C a -p hi --debug-file /workspace/.dbg | tail -2 | sed 's/^/    | /'
  wait "$WP"
  sed 's/^/    /' "$RUN/watch.txt"
  grep -aiE 'invalid_grant|refresh.*fail|blank|storage|secureStorage' "$S/ws-a/.dbg" | cut -c1-170 | head -6 | sed 's/^/    > /'
  say "  after: cred $(cred_shape "$FAKE/cred/.credentials.json")"
  for w in a b; do say "  $w auth status: $(XT=40 C "$w" auth status --json | jq -c '{loggedIn}' 2>/dev/null)"; done
  if [ -n "${HOLD:-}" ]; then kill "$TOUCHER" 2>/dev/null; wait "$TOUCHER" 2>/dev/null; rmdir "$FAKE/cred/.storage-write.lock" 2>/dev/null; fi
  say "  the shared cred dir: $(tree "$FAKE/cred")"
  for w in a b; do say "  cfg-k$w: $(tree "$(cfgdir "k$w")" | tr ';' '\n' | grep -E 'credential|lock|storage' | tr '\n' ';')"; done
  ;;
# ---------------------------------------------------------------------------
bump)
  FAIL=0
  pass() { say "  PASS  $*"; }
  fail() { say "  FAIL  $*"; FAIL=1; }
  fake_setup
  say "--- 1. the binary still names the variable"
  if grep -aqF 'CLAUDE_SECURESTORAGE_CONFIG_DIR' "$CLAUDE"; then pass "CLAUDE_SECURESTORAGE_CONFIG_DIR is in the binary"
  else fail "CLAUDE_SECURESTORAGE_CONFIG_DIR is gone from the binary"; fi
  say "--- 1b. tripwires (text in the binary, not behaviour): the credential write lock's name, and the --bg job allowlist's special case for the variable"
  # Behaviour these stand for is measured elsewhere (HOLD=1 blank; workers of
  # --bg jobs read the credential in seed/two). They are here so a bump that
  # renames either is looked at; their minified surroundings are not matched.
  if grep -aqF '".storage-write"' "$CLAUDE"; then pass "'.storage-write' (the credential write lock, <dir>/.storage-write.lock) is in the binary"
  else fail "'.storage-write' is gone: re-measure the write lock (HOLD=1 ./run.sh blank)"; fi
  if grep -aqF '!=="CLAUDE_SECURESTORAGE_CONFIG_DIR")continue' "$CLAUDE"; then pass "the --bg job environment allowlist still special-cases the variable"
  else fail "the --bg allowlist's case for the variable is gone: re-read it (./run.sh read) and re-run seed"; fi
  if grep -aqF '!=="CLAUDE_SECURESTORAGE_CONFIG_DIRX")continue' "$CLAUDE"; then fail "control: a string not in the binary was found; the tripwires prove nothing"
  else pass "control: a misspelled form of the same text is not found (the tripwire can fail)"; fi
  say "--- 2. the credential is read from the variable's dir (valid fake there, none in the config)"
  fake_cred "$FAKE/cred/.credentials.json" valid
  ACCT=$FAKE/acct.json mkcfg ba seeded
  ctr_up a "$(cfgdir ba)" "$FAKE/cred" "${BLACKHOLE[@]}" || exit 1
  st=$(XT=40 C a auth status --json)
  if printf '%s' "$st" | jq -e '.loggedIn == true' >/dev/null 2>&1; then pass "signed in from $IN_CRED/.credentials.json"
  else fail "not signed in with the credential in the variable's dir: $(printf '%s' "$st" | head -c 200)"; fi
  say "--- 2c. control: the variable's dir empty, the credential in CLAUDE_CONFIG_DIR instead"
  mv "$FAKE/cred/.credentials.json" "$(cfgdir ba)/.credentials.json"
  st=$(XT=40 C a auth status --json)
  if printf '%s' "$st" | jq -e '.loggedIn == false' >/dev/null 2>&1; then pass "a credential in CLAUDE_CONFIG_DIR is not read while the variable is set"
  else fail "the credential in CLAUDE_CONFIG_DIR was read despite the variable: $(printf '%s' "$st" | head -c 200)"; fi
  rm -f "$(cfgdir ba)/.credentials.json"
  say "--- 3. the account record is read from CLAUDE_CONFIG_DIR's .claude.json, not the variable's dir (a decoy with another organization planted there)"
  fake_cred "$FAKE/cred/.credentials.json" valid
  DECOY_ORG=00000000-0000-4000-8000-0000000000d0
  printf '{"oauthAccount":{"accountUuid":"00000000-0000-4000-8000-00000000000d","emailAddress":"decoy@example.invalid","organizationUuid":"%s"}}\n' "$DECOY_ORG" > "$FAKE/cred/.claude.json"
  chmod 600 "$FAKE/cred/.claude.json"; D0=$(sha256sum < "$FAKE/cred/.claude.json")
  st=$(XT=40 C a auth status --json)
  org=$(printf '%s' "$st" | jq -r .orgId 2>/dev/null)
  if [ "$org" = "$(jq -r .organizationUuid "$FAKE/acct.json")" ]; then pass "orgId is the one seeded into $IN_CFG/.claude.json"
  elif [ "$org" = "$DECOY_ORG" ]; then fail "orgId is the decoy's: the account record is read from the variable's dir"
  else fail "orgId is not the seeded one: $(printf '%s' "$st" | jq -c '{orgId}' 2>/dev/null)"; fi
  [ "$(sha256sum < "$FAKE/cred/.claude.json")" = "$D0" ] && pass "the decoy .claude.json in the variable's dir is untouched" || fail "something wrote the .claude.json in the variable's dir"
  rm -f "$FAKE/cred/.claude.json"
  say "--- 4. a refresh takes .oauth_refresh.lock in the variable's dir (expired fake, token host blackholed)"
  fake_cred "$FAKE/cred/.credentials.json" expired
  watch_locks 12 "$RUN/watch-var.txt" "$FAKE/cred" "$(cfgdir ba)" &
  WP=$!; sleep 1
  XT=11 C a -p hi >/dev/null; wait "$WP"
  sed 's/^/    /' "$RUN/watch-var.txt"
  if grep -q 'fake/cred/.oauth_refresh.lock(' "$RUN/watch-var.txt"; then pass "the refresh lock was taken in $IN_CRED"
  else fail "no refresh lock appeared in the variable's dir"; fi
  # Any lock entry in the config dir but Claude Code's own .claude.json.lock,
  # whatever its name: a bump that moved the refresh lock may rename it too.
  if grep -oE 'cfg-ba/[^ (]+\(' "$RUN/watch-var.txt" | grep -qv '/\.claude\.json\.lock($'; then fail "a lock appeared in CLAUDE_CONFIG_DIR during the refresh: the credential and its lock have come apart"
  else pass "no refresh lock in CLAUDE_CONFIG_DIR (no lock entry there but .claude.json.lock)"; fi
  docker rm -f "$(ctr a)" >/dev/null
  # The timeout ends the exec client, not the claude inside, so the container's
  # removal abandons a held lock. This is the fake dir, so clear it (never a
  # real volume's: CLAUDE.md, Spike 00).
  rm -rf "$FAKE/cred/.oauth_refresh.lock" "$FAKE/cred/.oauth_refresh.lock.owner"
  say "--- 4c. control: no variable, the same expired fake in CLAUDE_CONFIG_DIR — the watch sees a lock there"
  ACCT=$FAKE/acct.json mkcfg bb seeded "$FAKE/cred/.credentials.json"
  ctr_up b "$(cfgdir bb)" - "${BLACKHOLE[@]}" || exit 1
  watch_locks 12 "$RUN/watch-novar.txt" "$FAKE/cred" "$(cfgdir bb)" &
  WP=$!; sleep 1
  XT=11 C b -p hi >/dev/null; wait "$WP"
  sed 's/^/    /' "$RUN/watch-novar.txt"
  # By any name, as in 5: this is the watch's control, not the lock's.
  if grep -oE 'cfg-bb/[^ (]+\(' "$RUN/watch-novar.txt" | grep -v '\.owner($' | grep -qv '/\.claude\.json\.lock($'; then pass "without the variable the lock is in CLAUDE_CONFIG_DIR (the watch can see one)"
  else fail "the control saw no lock: the watch is blind, so 4 proves nothing"; fi
  docker rm -f "$(ctr b)" >/dev/null
  say "--- 5. the shared layout under the variable: ONE dir mounted at both $IN_CFG and $IN_CRED (what flag=shared would run)"
  fake_setup; ACCT=$FAKE/acct.json mkcfg bc seeded; fake_cred "$(cfgdir bc)/.credentials.json" valid
  ctr_up c "$(cfgdir bc)" "$(cfgdir bc)" "${BLACKHOLE[@]}" || exit 1
  st=$(XT=40 C c auth status --json)
  if printf '%s' "$st" | jq -e --arg o "$(jq -r .organizationUuid "$FAKE/acct.json")" '.loggedIn == true and .orgId == $o' >/dev/null 2>&1; then pass "signed in, with the seeded account, from the one dir"
  else fail "the one-dir layout does not sign in: $(printf '%s' "$st" | head -c 200)"; fi
  fake_cred "$(cfgdir bc)/.credentials.json" expired
  watch_locks 12 "$RUN/watch-onedir.txt" "$(cfgdir bc)" &
  WP=$!; sleep 1
  XT=11 C c -p hi >/dev/null; wait "$WP"
  sed 's/^/    /' "$RUN/watch-onedir.txt"
  # By any name (not the .owner record, not .claude.json.lock): with one dir
  # at both paths the lock is beside the credential wherever the bump puts it.
  if grep -oE 'cfg-bc/[^ (]+\(' "$RUN/watch-onedir.txt" | grep -v '\.owner($' | grep -qv '/\.claude\.json\.lock($'; then pass "the refresh lock is in the one dir, beside the credential"
  else fail "no refresh lock in the one dir"; fi
  if [ "$FAIL" = 0 ]; then say "BUMP CHECK PASSED: under CLAUDE_SECURESTORAGE_CONFIG_DIR the credential and its refresh lock share one dir, and the account record stays in CLAUDE_CONFIG_DIR"
  else say "BUMP CHECK FAILED: the split layout is not safe on this Claude Code; keep (or fall back to) the shared layout"; exit 1; fi
  ;;
# ---------------------------------------------------------------------------
seed)
  # s: oauthAccount seeded; u: none (the control); e: none, but the three
  # CLAUDE_CODE_* variables that pre-seed it.
  mkcfg ss seeded; mkcfg su bare; mkcfg se bare
  EV=(-e CLAUDE_CODE_ACCOUNT_UUID="$(jq -r .accountUuid "$S/oauthAccount.json")"
      -e CLAUDE_CODE_USER_EMAIL="$(jq -r .emailAddress "$S/oauthAccount.json")"
      -e CLAUDE_CODE_ORGANIZATION_UUID="$(jq -r .organizationUuid "$S/oauthAccount.json")")
  ctr_up s "$(cfgdir ss)" "$CRED" || exit 1
  ctr_up u "$(cfgdir su)" "$CRED" || exit 1
  ctr_up e "$(cfgdir se)" "$CRED" "${EV[@]}" || exit 1
  C0=$(sha256sum < "$CRED/.credentials.json")
  for w in s u e; do say "  $w before: oauthAccount $(acct "$(cfgdir "s$w")")"; done
  touch "$RUN/t0"
  declare -A J B
  for w in s u e; do
    say "--- $w: claude --bg --remote-control ddspike09$w"
    J[$w]=$(bg "$w" --remote-control "ddspike09$w")
  done
  for w in s u e; do B[$w]=$(bsid "$w" "$(cfgdir "s$w")" "${J[$w]}"); say "  $w: job ${J[$w]:-none}, Remote Control session: ${B[$w]}"; done
  sleep 20
  say "--- 30 s and more after the start"
  for w in s u e; do
    say "  $w: oauthAccount now $(acct "$(cfgdir "s$w")"); auth status $(C "$w" auth status --json | jq -c '{loggedIn, org: (.orgId != null)}' 2>/dev/null)"
    say "  $w: state.json $(jq -c '{state, bridgeSessionId}' "$(cfgdir "s$w")/jobs/${J[$w]}/state.json" 2>/dev/null)"
  done
  say "  profileFetchedAt in the seeded record was $(jq -r .profileFetchedAt "$S/oauthAccount.json") (the source login's)"
  say "--- u (no seed): stop, then respawn, now that a record may be there"
  C u stop "${J[u]}" | clean | sed 's/^/    | /'; sleep 3
  C u respawn "${J[u]}" | clean | sed 's/^/    | /'
  say "  u after respawn: session $(bsid u "$(cfgdir su)" "${J[u]}")"
  say "--- u: a second, new job"
  J2=$(bg u --remote-control ddspike09u2); say "  u: job ${J2:-none}, session $(bsid u "$(cfgdir su)" "$J2")"
  say "--- where everything is"
  say "  shared cred dir: $(tree "$CRED")"
  say "  credential unchanged by the run: $([ "$(sha256sum < "$CRED/.credentials.json")" = "$C0" ] && echo yes || echo NO)"
  for w in s u e; do say "  cfg-s$w: $(tree "$(cfgdir "s$w")")"; done
  for w in s u e; do say "  cfg-s$w has .credentials.json: $([ -e "$(cfgdir "s$w")/.credentials.json" ] && echo YES || echo no)"; done
  say "  files changed under the cred dir since the start: $(cd "$CRED" && find . -newer "$RUN/t0" | tr '\n' ' ')"
  for w in s u e; do say "  $w daemon.log auth lines:"; grep -aE 'auth:' "$(cfgdir "s$w")/daemon.log" 2>/dev/null | cut -c1-160 | head -3 | sed 's/^/    > /'; done
  [ -n "$J2" ] && C u stop "$J2" >/dev/null
  ;;
# ---------------------------------------------------------------------------
server)
  # Today's model (design §8) under split: the plain server, seeded and not,
  # with the argv Drydock's supervisor runs (internal/container/session.go).
  mkcfg rs seeded; mkcfg ru bare
  ctr_up s "$(cfgdir rs)" "$CRED" || exit 1; ctr_up u "$(cfgdir ru)" "$CRED" || exit 1
  for w in s u; do
    pane "srv$w" "$RUN/srv$w.log" docker exec -it -u vscode -w /workspace "$(ctr "$w")" claude remote-control --spawn worktree --capacity 4 --verbose
  done
  sleep 20
  for w in s u; do
    say "--- $w ($( [ "$w" = s ] && echo seeded || echo 'not seeded: the control')): claude remote-control"
    say "  environment id: $(grep -aoE 'env_[A-Za-z0-9]+' "$RUN/srv$w.log" | sort -u | tr '\n' ' ')"
    clean < "$RUN/srv$w.log" | grep -ahoE 'Capacity: [0-9]+/[0-9]+|Unable to determine[^.]*|Error: [^ ].{0,100}' | sort -u | sed 's/^/    | /'
    say "  oauthAccount in its config now: $(acct "$(cfgdir "r$w")")"
  done
  srv_stop() {
    local w P
    for w in s u; do
      P=$(ps_in "$w" | awk '/claude remote-control/ && !/docker/ {print $1; exit}')
      [ -n "$P" ] || { say "  $w: no server process left to stop"; continue; }
      docker exec -u vscode "$(ctr "$w")" kill -TERM "$P"; sleep 5
      say "  $w: SIGTERM of server $P: still running? $(docker exec "$(ctr "$w")" kill -0 "$P" 2>/dev/null && echo yes || echo no)"
    done
  }
  srv_stop
  say "--- the same server started again in each (the restart a supervisor would make)"
  for w in s u; do
    pane "srv$w" "$RUN/srv$w-2.log" docker exec -it -u vscode -w /workspace "$(ctr "$w")" claude remote-control --spawn worktree --capacity 4 --verbose
  done
  sleep 20
  for w in s u; do
    say "  $w, restarted: environment id: $(grep -aoE 'env_[A-Za-z0-9]+' "$RUN/srv$w-2.log" | sort -u | tr '\n' ' ')"
    clean < "$RUN/srv$w-2.log" | grep -ahoE 'Capacity: [0-9]+/[0-9]+|Unable to determine[^.]*|Error: [^ ].{0,100}' | sort -u | sed 's/^/    | /'
    say "  oauthAccount in its config now: $(acct "$(cfgdir "r$w")")"
  done
  srv_stop
  say "  shared cred dir: $(tree "$CRED")"
  ;;
# ---------------------------------------------------------------------------
two)
  mkcfg ta seeded; mkcfg tb seeded
  ctr_up a "$(cfgdir ta)" "$CRED" || exit 1; ctr_up b "$(cfgdir tb)" "$CRED" || exit 1
  CA=$(cfgdir ta); CB=$(cfgdir tb)
  say "--- A starts a job (A's daemon)"
  JA=$(bg a --remote-control twoa); [ -n "$JA" ] || exit 1
  BA=$(bsid a "$CA" "$JA"); say "  A's job $JA, session $BA"
  say "  B, before it runs anything: claude daemon status"; C b daemon status | clean | sed 's/^/    b| /' | head -6
  say "  B: claude agents --json --all: [$(agents b | tr '\n' ' ')]"
  say "  B: claude logs $JA:"; C b logs "$JA" | clean | head -2 | cut -c1-160 | sed 's/^/    b| /'
  say "--- B starts a job"
  T=$(now)
  JB=$(bg b --remote-control twob); [ -n "$JB" ] || exit 1
  BB=$(bsid b "$CB" "$JB"); say "  B's job $JB, session $BB"
  sleep 5
  say "  daemons: A $(dpid a), B $(dpid b)"
  say "  A lists:"; agents a | sed 's/^/    a| /'
  say "  B lists:"; agents b | sed 's/^/    b| /'
  say "  A's config:"; state "$CA"; say "  B's config:"; state "$CB"
  say "  A's daemon.log since B started:"; dlog_since "$CA" "$T"
  say "  B's daemon.log:"; dlog_since "$CB" "$T"
  say "--- 30 s on"
  sleep 30
  say "  A lists: $(agents a | tr '\n' ' ')"; say "  B lists: $(agents b | tr '\n' ' ')"
  say "  A's job: worker $(wpid a "$JA"), session $(BSID_WAIT=3 bsid a "$CA" "$JA") (was $BA); B's job: worker $(wpid b "$JB"), session $(BSID_WAIT=3 bsid b "$CB" "$JB") (was $BB)"
  say "--- B: claude stop $JA (A's job, from B)"
  C b stop "$JA" | clean | sed 's/^/    b| /'; sleep 3
  say "  A's job in A: worker $(wpid a "$JA"), $(agents a | grep -a "$JA")"
  say "--- B: claude respawn $JA"
  C b respawn "$JA" | clean | sed 's/^/    b| /'; sleep 8
  say "  processes in B:"; ps_in b | grep -a 'claude' | sed 's/^/    b| /'
  say "  B lists: $(agents b | tr '\n' ' ')"
  say "--- A: claude stop $JA; A respawns it: only A's changes?"
  C a stop "$JA" | clean | sed 's/^/    a| /'; sleep 3
  say "  B's job after A's stop: worker $(wpid b "$JB"), session $(BSID_WAIT=3 bsid b "$CB" "$JB")"
  C a respawn "$JA" | clean | sed 's/^/    a| /'
  say "  A's job after respawn: session $(bsid a "$CA" "$JA") (was $BA); B's: $(BSID_WAIT=3 bsid b "$CB" "$JB")"
  say "--- both stop their own; do both daemons exit?"
  C a stop "$JA" >/dev/null; C b stop "$JB" >/dev/null
  for i in $(seq 1 20); do [ -z "$(dpid a)$(dpid b)" ] && break; sleep 1; done
  say "  after ${i} s: daemon A '$(dpid a)', daemon B '$(dpid b)'"
  say "  shared cred dir: $(tree "$CRED")"
  ;;
# ---------------------------------------------------------------------------
attach)
  mkcfg aa seeded; ctr_up a "$(cfgdir aa)" "$CRED" || exit 1
  ID=$(bg a --remote-control ddspike09attach --debug-file /workspace/.w.debug); [ -n "$ID" ] || exit 1
  B0=$(bsid a "$(cfgdir aa)" "$ID"); say "  job $ID, session $B0"
  say "--- claude attach $ID from a second exec (a TTY, in tmux)"
  pane att "$RUN/attach.log" docker exec -it -u vscode -w /workspace "$(ctr a)" claude attach "$ID"
  sleep 8
  say "  attach screen:"; screen att | grep -v '^\s*$' | tail -5 | cut -c1-160 | sed 's/^/    | /'
  n0=$(grep -acE 'remote-bridge|bridge.*(send|sent|event)' "$S/ws-a/.w.debug")
  typeline att 'Reply with exactly TERM1 and nothing else.' 30
  say "  screen after the turn:"; screen att | grep -v '^\s*$' | tail -6 | cut -c1-160 | sed 's/^/    | /'
  say "  TERM1 in claude logs: $(C a logs "$ID" | clean | grep -ac 'TERM1')"
  say "  TERM1 in the session transcript (the conversation the app is sent): $(grep -rlaF 'TERM1' "$(cfgdir aa)/projects" 2>/dev/null | wc -l) file(s); its records naming TERM1: $(grep -rahF 'TERM1' "$(cfgdir aa)/projects" 2>/dev/null | jq -r '.type' 2>/dev/null | sort | uniq -c | tr '\n' ' ')"
  say "  worker debug lines about the bridge, before the turn $n0, after $(grep -acE 'remote-bridge|bridge.*(send|sent|event)' "$S/ws-a/.w.debug")"
  grep -aE 'remote-bridge' "$S/ws-a/.w.debug" | tail -4 | cut -c1-170 | sed 's/^/    > /'
  say "--- the attach client killed (tmux session killed): does the session go on?"
  t kill-session -t att; sleep 5
  say "  job: worker $(wpid a "$ID"), session $(BSID_WAIT=3 bsid a "$(cfgdir aa)" "$ID") (was $B0); agents: $(agents a)"
  C a stop "$ID" >/dev/null
  ;;
# ---------------------------------------------------------------------------
supervise)
  mkcfg va seeded; CA=$(cfgdir va); ctr_up a "$CA" "$CRED" || exit 1
  say "--- claude daemon run, held in the foreground by a docker exec (the supervisor's process)"
  pane fgd "$RUN/fgd.log" docker exec -u vscode -w /workspace "$(ctr a)" claude daemon run
  sleep 4; D=$(dpid a); say "  daemon ${D:-none}: $(C a daemon status | clean | grep -aE 'origin' | head -1)"
  ID=$(bg a --remote-control ddspike09sup --debug-file /workspace/.w.debug); [ -n "$ID" ] || exit 1
  B0=$(bsid a "$CA" "$ID"); W=$(wpid a "$ID")
  say "  job $ID, worker $W; the link: claude.ai/code/$B0 (sessions/$W.json bridgeSessionId), state.json: $(jq -r .bridgeSessionId "$CA/jobs/$ID/state.json" 2>/dev/null)"
  say "=== claude stop"
  : > "$S/ws-a/.w.debug"; C a stop "$ID" | clean | sed 's/^/    | /'; sleep 3
  grep -aE 'Archive|Torn down|teardown' "$S/ws-a/.w.debug" | tail -2 | cut -c1-150 | sed 's/^/    > /'
  say "  daemon still up (foreground): '$(dpid a)'"
  say "=== claude respawn"
  : > "$S/ws-a/.w.debug"; C a respawn "$ID" | clean | sed 's/^/    | /'
  B1=$(bsid a "$CA" "$ID"); say "  session $B1 (was $B0): $([ "$B1" = "$B0" ] && echo same || echo DIFFERENT)"
  grep -aE 'Reattaching|Unarchive|Created session' "$S/ws-a/.w.debug" | tail -2 | cut -c1-150 | sed 's/^/    > /'
  say "=== docker stop, then docker start, with the job live"
  : > "$S/ws-a/.w.debug"; docker stop -t 10 "$(ctr a)" >/dev/null
  say "  teardown lines (none = no teardown ran): $(grep -acE 'Archive|Torn down' "$S/ws-a/.w.debug")"
  state "$CA"; say "  shared cred dir after the stop: $(tree "$CRED")"
  docker start "$(ctr a)" >/dev/null; sleep 3
  say "  back up; daemon before anything asks: '$(dpid a)'"
  pane fgd "$RUN/fgd2.log" docker exec -u vscode -w /workspace "$(ctr a)" claude daemon run; sleep 4
  C a respawn "$ID" | clean | sed 's/^/    | /'
  B2=$(bsid a "$CA" "$ID"); say "  session after respawn $B2 (was $B0): $([ "$B2" = "$B0" ] && echo same || echo DIFFERENT)"
  grep -aE 'Reattaching|Unarchive|Created session' "$S/ws-a/.w.debug" | tail -2 | cut -c1-150 | sed 's/^/    > /'
  say "=== claude daemon stop --any"
  : > "$S/ws-a/.w.debug"; C a daemon stop --any | clean | sed 's/^/    | /'; sleep 5
  grep -aE 'Archive|Torn down' "$S/ws-a/.w.debug" | tail -2 | cut -c1-150 | sed 's/^/    > /'
  say "  daemon: '$(dpid a)'; worker: '$(wpid a "$ID")'"
  say "  daemon.log refresh scheduling: $(grep -a 'scheduling proactive refresh' "$CA/daemon.log" | tail -1 | cut -c1-120)"
  ;;
# ---------------------------------------------------------------------------
app)
  mkcfg pa seeded; CA=$(cfgdir pa); ctr_up a "$CA" "$CRED" || exit 1
  pane fgd "$RUN/fgd.log" docker exec -u vscode -w /workspace "$(ctr a)" claude daemon run
  sleep 4
  ID=$(bg a --remote-control ddspike09app --debug-file /workspace/.w.debug); [ -n "$ID" ] || exit 1
  B0=$(bsid a "$CA" "$ID")
  say "H1a (the phone): open https://claude.ai/code/$B0 (or find 'ddspike09app' in the app)."
  say "     Send: Reply with exactly APP1 and nothing else.   Waiting $W_APP s."
  sleep "$W_APP"
  say "  APP1 in claude logs: $(C a logs "$ID" | clean | grep -ac APP1)"
  say "H1b (the laptop): in a terminal on this machine run"
  say "       docker exec -it -u vscode -w /workspace $(ctr a) claude attach $ID"
  say "     confirm APP1 is in the history, type: Reply with exactly TERM2 and nothing else."
  say "     Then look at the phone: is TERM2 there, as a turn of the same session? Detach (or close the terminal)."
  say "     Waiting $W_APP s."
  sleep "$W_APP"
  say "  seen in claude logs: $(C a logs "$ID" | clean | grep -aoE 'APP1|TERM2' | sort -u | tr '\n' ' ')"
  C a daemon stop --any | clean | sed 's/^/    | /'
  ;;
# ---------------------------------------------------------------------------
read)
  ctx() { # ctx <string> <before> <after>: the source text (not bytecode copies) around each hit
    grep -aboF "$1" "$CLAUDE" | cut -d: -f1 | while read -r off; do
      dd if="$CLAUDE" bs=1 skip=$(( off - $2 )) count=$(( $2 + $3 )) 2>/dev/null | tr -c '[:print:]' '.' > "$RUN/ctx.$off"
      grep -aqE 'function|=>|let |await ' "$RUN/ctx.$off" || continue
      say "--- '$1' at byte $off"; { fold -w 160 "$RUN/ctx.$off"; echo; } | sed 's/^/    /'
    done
  }
  say "=== the resolver: the variable if set (empty means ~/.claude), else CLAUDE_CONFIG_DIR's dir"
  ctx 'function HS(){let e=process.env.CLAUDE_SECURESTORAGE_CONFIG_DIR' 0 260
  say "=== the plaintext credential store: <HS()>/.credentials.json"
  ctx 'function i(){let e=HS(),r=".credentials.json"' 0 160
  say "=== the credential write lock: <HS()>/.storage-write"
  ctx 'B(o,".storage-write")' 120 200
  say "=== the refresh: the lock on <HS()>/.oauth_refresh.lock, and the legacy lock <realpath(HS())>.lock"
  ctx 'let K=HS();await se().mkdir(K);let V;try{' 60 300
  ctx 'function dkn(e,n){return{lockfilePath:Hf(e,".oauth_refresh.lock")' 0 420
  say "=== the --bg job environment allowlist carries the variable to workers (kept even when empty)"
  ctx 'if(o===""&&r!=="CLAUDE_SECURESTORAGE_CONFIG_DIR")continue' 200 100
  say "=== a reader that does NOT use HS(): a session-store resume copies \$CLAUDE_CONFIG_DIR/.credentials.json"
  ctx 'E=await cn(x(M,".credentials.json"),"utf-8")' 300 300
  say "=== the account record: refetched when profileFetchedAt is older than RK, and the env pre-seed"
  ctx 'S.oauthAccount?.profileFetchedAt,w=E!==void 0&&Date.now()-E<RK' 360 200
  ctx 'var RK=86400000' 0 40
  say "=== the organization lookup API calls use: CLAUDE_CODE_ORGANIZATION_UUID first, then the record, then the profile"
  ctx 'let e=a.CLAUDE_CODE_ORGANIZATION_UUID;if(e)return e;let r=le().oauthAccount?.organizationUuid' 25 220
  say "=== a refresher that finds the lock held (ELOCKED): up to five retries 1-2 s apart, then a dead-holder takeover check"
  ctx 'if(Z.code==="ELOCKED"){let J=s??await xf(Z,K);if(n<5)' 0 260
  ;;
esac
say "=== done ($MODE)"
