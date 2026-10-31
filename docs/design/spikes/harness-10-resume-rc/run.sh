#!/usr/bin/env bash
# Spike 10 — can a terminal take over a conversation started from the app
# against the workspace's `claude remote-control` server, with its history,
# and keep it in the app? See ../10-resume-rc-handoff.md.
#
# Usage: S=<scratch> ./run.sh <mode>     (build <scratch> with ./setup.sh first)
#
#   cli       what 2.1.289 says about --resume/--continue/--fork-session/--rc,
#             and the bridge's history-upload and refusal strings
#   handoff   the server as Drydock runs it; history put into its session; then,
#             with the server live, each hand-off variant in turn:
#               (a) --resume <id> --rc      (b) --resume <id> --fork-session --rc
#               (c) --continue --rc         (d) --resume <id>, then /remote-control
#             then (e) the server SIGTERMed and (a) again, (e2) the server
#             restarted beside it, and (f) a second (a) while (e) holds it.
#             Unattended, the history is one turn typed in a terminal that
#             `--continue` opened on the server's session (a stand-in: only
#             the app can send a turn to a server session). With WAIT_APP=<s>
#             it waits that long for the owner to send the turn from the app
#             instead, and at (a) and (b) for app turns (H1-H4 in the README).
#             Needs a fresh scratch each run (it refuses a used one).
#   picker    handoff's server and history, then `claude --resume --fork-session
#             --rc` with no id (Claude Code's picker; a bare trailing --rc, the
#             docs sentence's exact form), Enter on the first row. Fresh
#             scratch too.
#   takeover  handoff's server and history, then (e) --resume <id> --rc and
#             (f) a second one while (e) holds it; (f) is stopped and (e)
#             watched: was it displaced while (f) lived, and when did it drop.
#             Fresh scratch too.
#   ids       from the app session id Drydock records (session_…, from the
#             server's OSC 8 links) to the local id --resume takes, and the
#             uuidv5 the binary describes, computed
#   worktree  where --resume finds a conversation started in a `claude -w`
#             worktree, from the clone's path, the worktree's, and $HOME, and
#             the reverse; and where --fork-session puts it (by id from the
#             clone and from the worktree, from the clone's picker after
#             Ctrl+W, and from the worktree's picker). The picker rows need a
#             fresh scratch; on a used one they print NOT MEASURED.
#
# Every mode reaps what it started (SIGTERM first; Spike 06's lib.sh), then
# sweeps every process still carrying CLAUDE_CONFIG_DIR=$CFG; after a SIGKILL
# of this script, ./reap.sh does both. Turns are one-word replies, at most
# ten per mode: they spend the account's quota.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODE="${1:?usage: S=<scratch> ./run.sh <mode>}"
case "$MODE" in
  cli|handoff|picker|takeover|ids|worktree) ;;
  *) printf 'unknown mode: %s\n' "$MODE"; exit 2 ;;
esac
: "${S:?set S to the scratch dir setup.sh built}"
# WAIT_APP: seconds each human step may take (0, the default, is unattended).
# Validated before any arithmetic; it is counted into the token guard's
# minutes, since a wait on the owner is time the copied token keeps ageing.
W_APP="${WAIT_APP:-0}"
is_uint() { case $1 in ''|0?*|*[!0123456789]*) return 1 ;; esac; [ "${#1}" -le 15 ]; }
is_uint "$W_APP" || { printf 'REFUSING: WAIT_APP=%s is not a whole number of seconds (no leading zero, at most 15 digits)\n' "$W_APP"; exit 2; }
# The minutes each mode may run: 15 unattended (the longest, handoff, takes
# about 9), and handoff waits up to four times on the owner (H1-H4). The other
# live modes have no human step but H1's, which they do not offer: refuse it.
case "$MODE" in
  handoff) MODE_MIN=$(( (4 * W_APP + 59) / 60 + 15 )) ;;
  *) [ "$W_APP" -eq 0 ] || { printf 'REFUSING: WAIT_APP is for handoff only (the human steps are there)\n'; exit 2; }
     MODE_MIN=15 ;;
esac
RUN="$S/run/$MODE"
# lib.sh refuses a scratch that is the real config or $HOME before anything
# is created or deleted; only then is this mode's run directory emptied.
. "$HERE/lib.sh"
rm -rf "$RUN"; mkdir -p "$RUN"
# The guard runs before the trap: a refused run starts and stops nothing.
token_guard "$CFG/.credentials.json" "$MODE_MIN"
trap reap EXIT
# A killed run (timeout, Ctrl-C) must still reap: bash runs the EXIT trap on a
# signal only when that signal is trapped. ./reap.sh cleans up after a SIGKILL.
trap 'exit 143' TERM; trap 'exit 130' INT; trap 'exit 129' HUP
say "claude under test: $(cenv "$CLAUDE" --version 2>&1 | head -1)"
say "ws=$WS cfg=$CFG run=$RUN marker=$MARKER"
reap

# variant <tag> <dir> <claude args...>: start a hand-off terminal and report
# what it is: its markers, the app session it holds, the local conversation it
# is on, what the bridge did, whether it has the history, and the server.
variant() {
  local tag="$1" dir="$2"; shift 2
  local log="$RUN/$tag.log" dbg="$RUN/$tag.debug"
  T0=$(now)
  pane "$tag" "$dir" "$log" "$CLAUDE" "$@" --debug-file "$dbg"
  waitscreen 25 "$tag" 'remote-control is active|/rc active|Remote Control not started|No conversation found|Error' ; sleep 4
  say "  claude $*"
  mark "$tag" "$log"
  VB=$(rcline "$log"); VU=""
  [ -n "$VB" ] && VU=$(uuid_of "$VB")
  VP=$(panepid "$tag")
  say "  app session printed: ${VB:-none} (the server's is $B)  OSC 8 ids in its output: [$(sessids "$log")]"
  say "  its registry row: $(live_rows | jq -c --arg p "$VP" 'select((.pid|tostring) == $p) | {sessionId, bridgeSessionId, cwd}')"
  say "  the bridge, from its debug log:"; bridge "$dbg"
}

# takeover_watch <holder> <taker>: the taker has just taken the holder's app
# session over. Was the holder displaced while the taker lived, and when did it
# drop once the taker was stopped? Both teardowns' timestamps come from the
# debug logs (each line's first field), so the order is read, not inferred.
DROP_RE='4090|session_not_active'
takeover_watch() {
  local e="$1" f="$2" ep n0
  ep=$(panepid "$e")
  # 60 s with both alive, so a displacement that waits on (e)'s next request
  # (14 s in the first takeover run) lands well before (f) is signalled, and
  # the moment of the signal is printed to the millisecond beside it.
  say "  both alive from $(date -u +%H:%M:%S.%3N); waiting 60 s before stopping ($f)"
  sleep 60
  n0=$(grep -acE "$DROP_RE" "$RUN/$e.debug" 2>/dev/null)
  say "  while ($f) lives: ($e) pid $ep alive=$(kill -0 "$ep" 2>/dev/null && echo yes || echo no), $n0 '$DROP_RE' lines in its debug log"
  say "    its registry row: $(live_rows | jq -c --arg p "$ep" 'select((.pid|tostring) == $p) | {bridgeSessionId}')"
  grep -aE "$DROP_RE|superseded|another connection|taking over|state=" "$RUN/$e.debug" | cut -c1-200 | tail -4 | sed 's/^/    > /'
  say "  stopping ($f) (SIGTERM) at $(date -u +%H:%M:%S.%3N)"
  term "$f"
  timeout 60 bash -c "until ! kill -0 $ep 2>/dev/null || grep -aqE '$DROP_RE' '$RUN/$e.debug'; do sleep 5; done"
  sleep 5
  say "  after: ($e) alive=$(kill -0 "$ep" 2>/dev/null && echo yes || echo no), $(grep -acE "$DROP_RE" "$RUN/$e.debug" 2>/dev/null) '$DROP_RE' lines (was $n0)"
  say "    ($f) teardown: $(grep -aE 'Torn down' "$RUN/$f.debug" | tail -1 | cut -c1-160)"
  say "    ($e) drop:     $(grep -aE "$DROP_RE" "$RUN/$e.debug" | head -1 | cut -c1-160)"
  mark "$e" "$RUN/$e.log"
}

case "$MODE" in
# ---------------------------------------------------------------------------
cli)
  say "=== what 2.1.289 offers for a hand-off"
  cenv "$CLAUDE" --help 2>&1 | grep -aE -A2 -- '^  -r, --resume|^  -c, --continue|--fork-session|^  --remote-control \[|^  -w, --worktree' | sed 's/^/    /'
  bin="$(readlink -f "$CLAUDE")"
  say "--- the bridge on a resumed conversation: history upload and its suppressions"
  strings -n 8 "$bin" | grep -aoE '\[bridge:repl\][^`"]{0,160}(history|backfill)[^`"]{0,80}' | sort -u | sed 's/^/    /'
  say "--- the persistence sync's own refusals to backfill (history not uploaded)"
  strings -n 8 "$bin" | grep -aoE '\[persistence-sync\] Refusing backfill[^`"]{0,160}' | sort -u | sed 's/^/    /'
  say "--- what --fork-session drops on a resume (the worktree binding, the relocated cwd)"
  strings -n 8 "$bin" | grep -aoE 'effectiveFork:[A-Za-z]+\}=[A-Za-z]+\([^)]{0,60}\);.{0,200}stripWorktreeSession:!0,stripRelocatedCwd:!0' | head -3 | cut -c1-320 | sed 's/^/    /'
  say "    resume paths that strip it when forking: $(strings -n 8 "$bin" | grep -aoE 'stripWorktreeSession:!0,stripRelocatedCwd:!0' | wc -l)"
  strings -n 8 "$bin" | grep -aoE 'The worktree binding has been cleared[^`"]{0,140}' | sort -u | sed 's/^/    /'
  say "--- the local id's derivation, as the binary describes it"
  strings -n 8 "$bin" | grep -aoE 'a uuidv5 of the session id[^"]{0,200}' | sort -u | sed 's/^/    /'
  say "--- refusals a resume can meet"
  strings -n 8 "$bin" | grep -aoE "(another Claude Code on this machine[^\`]{0,10}|already has Remote Control for this conversation|Can't open [^\"]{0,60}|cannot resume into worktree|to continue without the worktree, re-run with --fork-session[^\`]{0,40}|--session-id can only be used with --continue or --resume if --fork-session is also specified)" | sort -u | sed 's/^/    /'
  ;;
# ---------------------------------------------------------------------------
handoff|picker|takeover)
  say "=== the hand-off, against the server's own session"
  start_server "$RUN/srv.log" || exit 1
  B=$(sessids "$RUN/srv.log" | awk '{print $1}'); E=$(envid "$RUN/srv.log")
  U=$(uuid_of "$B") || { say "no registry row names $B"; exit 1; }
  F="$(projdir "$WS")/$U.jsonl"
  SDBG="$RUN/srv.log-${B/session_/cse_}.debug"
  [ -f "$SDBG" ] || { say "no session debug log at $SDBG"; exit 1; }
  srvstate "$RUN/srv.log"
  say "  server session: $B  local id: $U  transcript: $F"
  # The server's local id is stable across restarts on one config, so a second
  # run on the same scratch finds the first run's turns in it and every
  # result below would describe a used conversation. Refuse that.
  [ "$(turns "$F")" -eq 0 ] || { say "REFUSING: $F already has turns; run ./setup.sh on a fresh scratch for each handoff run"; exit 1; }

  say "--- history into the server's session"
  if [ "$W_APP" -gt 0 ]; then
    say "  HUMAN STEP H1: open https://claude.ai/code?environment=$E , open the session"
    say "  named $(clean "$RUN/srv.log" | grep -aoE 'ddspike10-[a-z]+-[a-z]+' | tail -1), and send:"
    say "    Remember the word $MARKER. Reply with exactly APP1 and nothing else."
    say "  waiting up to ${W_APP}s for that turn to reach $F"
    for _ in $(seq 1 "$W_APP"); do grep -aqF "$MARKER" "$F" 2>/dev/null && [ "$(turns "$F")" -ge 2 ] && break; sleep 1; done
    grep -aqF "$MARKER" "$F" 2>/dev/null && [ "$(turns "$F")" -ge 2 ] || { say "  no app turn carrying $MARKER arrived; stopping"; exit 1; }
  else
    # The stand-in. Spike 06: before a turn `--resume <id>` finds nothing,
    # but `--continue` in the folder opens the server's pre-created session.
    pane seed "$WS" "$RUN/seed.log" "$CLAUDE" --continue; sleep 12
    if holders "$U" | grep -q '"entrypoint":"cli"'; then
      typeline seed "Remember the word $MARKER. Reply with exactly OK0 and nothing else." 15
      say "  seeded through --continue: $(screen seed | grep -aE '^● ' | tail -1)"
    else
      say "  --continue did not open $U:"; mark seed "$RUN/seed.log"; registry | sed 's/^/    /'; exit 1
    fi
    term seed
  fi
  say "  transcript now: $(turns "$F") user/assistant records"; TREE "$F" | sed 's/^/    /'
  # A conversation that has held Remote Control carries a pointer to its app
  # session (`bridge-session` records); a later resume reattaches to it. Does
  # the server's own session leave one?
  say "  bridge-session records in it: [$(jq -rc 'select(.type == "bridge-session") | .bridgeSessionId' "$F" | sort -u | tr '\n' ' ')]"
  say "  does the transcript name the server's app session? $(grep -acF "${B#session_}" "$F") lines"
  say "  bridge-pointer.json: $(jq -c '{sessionId, environmentId, source}' "$(projdir "$WS")/bridge-pointer.json" 2>/dev/null)"

  if [ "$MODE" = picker ]; then
    # No id at all: Claude Code's own picker, with the hand-off flags. What it
    # lists, and what Enter on the first row (the most recent) gives.
    # The docs sentence's exact form: a bare --rc, last. --rc takes an optional
    # [name]; with nothing after it the session gets Claude Code's default
    # name, recorded below. (--debug-file goes first so it is not the name.)
    say "--- (p) claude --resume --fork-session --rc (the picker, a bare trailing --rc), the server live"
    pane vp "$WS" "$RUN/vp.log" "$CLAUDE" --debug-file "$RUN/vp.debug" --resume --fork-session --rc; sleep 10
    say "  the picker:"; screen vp | grep -aE '❯|ago|bytes|No sessions' | head -6 | cut -c1-150 | sed 's/^/    | /'
    t send-keys -t vp Enter
    waitscreen 25 vp 'remote-control is active|/rc active|Error' ; sleep 4
    mark vp "$RUN/vp.log"
    VB=$(rcline "$RUN/vp.log")
    say "  app session: ${VB:-none} (the server's $B)"
    say "  its registry row: $(live_rows | jq -c --arg p "$(panepid vp)" 'select((.pid|tostring) == $p) | {sessionId, bridgeSessionId, cwd}') (the server's local id $U)"
    say "  the bridge:"; bridge "$RUN/vp.debug" | grep -aE 'Created|Reattach|Flushing|history'
    say "  the name a bare --rc gives (registry row, screen, debug log):"
    say "    registry: $(live_rows | jq -c --arg p "$(panepid vp)" 'select((.pid|tostring) == $p) | {name, title, sessionName} | with_entries(select(.value != null))')"
    screen vp | grep -aiE 'remote control|/rc' | head -3 | cut -c1-150 | sed 's/^/    | /'
    grep -aiE '(session|bridge).{0,40}(name|title)' "$RUN/vp.debug" | cut -c26-200 | head -4 | sed 's/^/    > /'
    say "  history? asked for the marker: $(ask vp)"
    say "  the server's transcript: $(turns "$F") records (2 before)"
    srvstate "$RUN/srv.log"
    say "=== done ($MODE)"
    exit 0
  fi

  if [ "$MODE" = takeover ]; then
    # Result 6 on its own: one terminal holds the hand-off, a second asks for
    # it (the laptop opens another terminal), and the second is stopped.
    say "--- (e) claude --resume <id> --rc, the server live"
    variant ve "$WS" --resume "$U" --rc ddspike10e
    [ -n "$VB" ] && say "  history? asked for the marker: $(ask ve)"
    say "--- (f) a second --resume <id> --rc while (e) holds it"
    variant vf "$WS" --resume "$U" --rc ddspike10f
    takeover_watch ve vf
    srvstate "$RUN/srv.log"
    say "=== done ($MODE)"
    exit 0
  fi

  # human <tag>: with WAIT_APP, the app-side half of a variant.
  human() {
    [ "$W_APP" -gt 0 ] && [ -n "$VB" ] || return 0
    say "  HUMAN STEP H$1: open https://claude.ai/code/$VB . Does it show the earlier"
    say "  turn ($MARKER)? Is it in the environment or under \"Remote Control\"? Then send:"
    say "    Reply with exactly APP$1 and nothing else."
    say "  waiting up to ${W_APP}s for it to reach the terminal"
    for _ in $(seq 1 "$W_APP"); do screen "$2" | grep -q "APP$1" && break; sleep 1; done
    sleep 8; say "  terminal: $(screen "$2" | grep -aE '^(❯|●) ' | tail -3 | tr '\n' '|')"
    typeline "$2" "Reply with exactly TERM$1 and nothing else." 15
    say "  typed TERM$1 in the terminal: check it shows in the app session, and in the server's ($B)"
    sleep 20
  }

  say "--- (a) claude --resume <id> --rc, the server live and holding it"
  variant va "$WS" --resume "$U" --rc ddspike10a
  say "  processes holding $U:"; holders "$U" | sed 's/^/    /'
  if [ -n "$VB" ] || kill -0 "$VP" 2>/dev/null; then
    T1=$(now)
    say "  history? asked for the marker: $(ask va)"
    say "  the shared transcript after that turn:"; TREE "$F" | sed 's/^/    /'
    say "  the server session's process since:"; gained "$SDBG" "$T1"
    human 2 va
    if [ "$W_APP" -gt 0 ]; then
      # Two writers on one file: the server's session process and (a) both hold
      # $U. A turn into the *server's* app session now is the fork test.
      before=$(turns "$F")
      say "  HUMAN STEP H3: in the app, go back to the server's session (in the environment,"
      say "  $B) and send: Reply with exactly APP3 and nothing else."
      for _ in $(seq 1 "$W_APP"); do [ "$(turns "$F")" -gt "$before" ] && break; sleep 1; done
      sleep 8
      say "  the shared transcript now ($before -> $(turns "$F") records):"; TREE "$F" | sed 's/^/    /'
      say "  did APP3 reach the terminal (a)? $(screen va | grep -c 'APP3') screen lines"
    fi
  fi
  srvstate "$RUN/srv.log"; VA_B="$VB"
  term va

  say "--- (b) claude --resume <id> --fork-session --rc"
  before=$(turns "$F")
  variant vb "$WS" --resume "$U" --fork-session --rc ddspike10b
  if [ -n "$VU" ]; then
    say "  history? asked for the marker: $(ask vb)"
    say "  its own transcript ($VU):"; TREE "$(projdir "$WS")/$VU.jsonl" | sed 's/^/    /'
    say "  the server's transcript: $before -> $(turns "$F") records"
    human 4 vb
  fi
  srvstate "$RUN/srv.log"
  term vb

  say "--- (c) claude --continue --rc"
  variant vc "$WS" --continue --rc ddspike10c
  say "  it opened: $VU (server's $U; (a) held $VA_B)"
  [ -n "$VB" ] && say "  history? asked for the marker: $(ask vc)"
  term vc

  say "--- (d) claude --resume <id>, then /remote-control inside"
  pane vd "$WS" "$RUN/vd.log" "$CLAUDE" --resume "$U" --debug-file "$RUN/vd.debug"; sleep 14
  mark vd "$RUN/vd.log"
  say "  before /remote-control: holds bridge [$(live_rows | jq -r --arg p "$(panepid vd)" 'select((.pid|tostring) == $p) | .bridgeSessionId')]"
  typeline vd '/remote-control ' 15
  mark vd "$RUN/vd.log"
  say "  what /remote-control shows:"; screen vd | grep -v '^\s*$' | tail -8 | cut -c1-150 | sed 's/^/    | /'
  t send-keys -t vd Escape; sleep 2
  say "  app session: $(rcline "$RUN/vd.log") (the server's $B; (a) held $VA_B)"
  say "  the bridge:"; bridge "$RUN/vd.debug"
  say "  history? asked for the marker: $(ask vd)"
  srvstate "$RUN/srv.log"
  term vd

  say "--- the server's session process across (a)-(d): taken over?"
  dbg_since_any() { grep -aE "$2" "$1" | cut -c1-200 | tail -6 | sed 's/^/    > /'; }
  dbg_since_any "$SDBG" '4090|superseded|another connection|state=|ended'
  say "  its process alive: $(kill -0 "$(live_rows | jq -r --arg b "$B" 'select(.entrypoint == "sdk-cli" and .bridgeSessionId == $b) | .pid' | head -1)" 2>/dev/null && echo yes || echo no)"
  say "  the shared transcript at the end:"; TREE "$F" | sed 's/^/    /'

  say "--- (e) SIGTERM the server, then --resume <id> --rc"
  term srv; sleep 2
  variant ve "$WS" --resume "$U" --rc ddspike10e
  [ -n "$VB" ] && say "  history? asked for the marker: $(ask ve)"
  say "--- (e2) restart the server while (e) holds the conversation"
  start_server "$RUN/srv2.log" && srvstate "$RUN/srv2.log"
  sleep 20
  say "  (e) after the restart:"; mark ve "$RUN/ve.log"
  grep -aE '4090|superseded|another connection' "$RUN/ve.debug" | cut -c26-200 | tail -3 | sed 's/^/    > /'
  say "  processes holding $U:"; holders "$U" | sed 's/^/    /'
  say "--- (f) a second --resume <id> --rc while (e) holds it (the laptop opens a second terminal)"
  variant vf "$WS" --resume "$U" --rc ddspike10f
  takeover_watch ve vf
  say "  transcript, last:"; TREE "$F" | sed 's/^/    /'
  say "  bridge-session records in it: [$(jq -rc 'select(.type == "bridge-session") | .bridgeSessionId' "$F" | sort -u | tr '\n' ' ')]"
  ;;
# ---------------------------------------------------------------------------
ids)
  say "=== from the app's session id to the local id --resume needs"
  start_server "$RUN/srv.log" || exit 1
  B=$(sessids "$RUN/srv.log" | awk '{print $1}')
  U=$(uuid_of "$B") || { say "no registry row names $B"; exit 1; }
  say "  the server's tail gives (what Drydock's rc_session rows hold): $B"
  say "  the registry row naming it ($CFG/sessions/<pid>.json): local id $U"
  say "  the local id in the server's own tail or debug log? $(grep -alF "$U" "$RUN/srv.log" "$RUN/srv.log.debug" 2>/dev/null | wc -l) files"
  # The session's process is the server's child: its argv names both ids.
  CP=$(live_rows | jq -r --arg b "$B" 'select(.bridgeSessionId == $b and .entrypoint == "sdk-cli") | .pid' | head -1)
  A=$(tr '\0' '\n' < "/proc/$CP/cmdline")
  say "  the session process (pid $CP, parent $(ps -o ppid= -p "$CP" | tr -d ' ') = server $(panepid srv)):"
  say "    --session-id $(printf '%s\n' "$A" | grep -A1 -x -- '--session-id' | tail -1)"
  say "    --sdk-url    $(printf '%s\n' "$A" | grep -A1 -x -- '--sdk-url' | tail -1)"
  say "  UUID version nibble of the local id: ${U:14:1}"
  # The binary's account of the id (read), then the computation it implies
  # (measured): uuid5(namespace, name) for every UUID literal in the binary as
  # the namespace, over each name the session process was handed.
  bin="$(readlink -f "$CLAUDE")"
  say "--- the binary on the local id (read)"
  strings -n 8 "$bin" | grep -aoE 'a uuidv5 of the session id[^"]{0,200}' | sort -u | sed 's/^/    /'
  say "--- the uuidv5, computed over the binary's UUID literals"
  strings -n 8 "$bin" | grep -aoE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | sort -u > "$RUN/uuids"
  SDK=$(printf '%s\n' "$A" | grep -A1 -x -- '--sdk-url' | tail -1)
  RSID=$(tr '\0' '\n' < "/proc/$CP/environ" 2>/dev/null | sed -n 's/^CLAUDE_CODE_REMOTE_SESSION_ID=//p')
  say "  $(wc -l < "$RUN/uuids") distinct UUID literals; the session process's CLAUDE_CODE_REMOTE_SESSION_ID: ${RSID:-unset}"
  node - "$U" "$RUN/uuids" "cse=${B/session_/cse_}" "session=$B" "bare=${B#session_}" "sdk-url=$SDK" "env=$RSID" <<'JS'
const crypto = require('crypto'), fs = require('fs');
const [want, file, ...pairs] = process.argv.slice(2);
// RFC 4122 version 5, as the binary's own helper computes it.
const v5 = (ns, name) => {
  const h = crypto.createHash('sha1').update(Buffer.from(ns.replace(/-/g, ''), 'hex')).update(Buffer.from(name, 'utf8')).digest();
  h[6] = (h[6] & 15) | 80; h[8] = (h[8] & 63) | 128;
  const e = h.subarray(0, 16).toString('hex');
  return `${e.slice(0, 8)}-${e.slice(8, 12)}-${e.slice(12, 16)}-${e.slice(16, 20)}-${e.slice(20, 32)}`;
};
const names = [];
for (const p of pairs) {
  const i = p.indexOf('='), label = p.slice(0, i), name = p.slice(i + 1);
  if (!name) continue;
  names.push([label, name]);
  if (label === 'sdk-url') { try { names.push(['sdk-url as URL.href', new URL(name).href]); } catch {} }
}
let hits = 0;
for (const ns of fs.readFileSync(file, 'utf8').split('\n').filter(Boolean))
  for (const [label, name] of names)
    if (v5(ns, name) === want) { console.log(`    MATCH: uuid5(${ns}, <${label}>) = the local id`); hits++; }
if (!hits) console.log(`    no (namespace, name) pair of ${names.map(n => n[0]).join(', ')} gives the local id`);
JS
  say "--- does --resume take the app's id instead?"
  for id in "$B" "${B/session_/cse_}"; do
    pane rid "$WS" "$RUN/rid.log" "$CLAUDE" --resume "$id"; sleep 10
    say "  claude --resume $id"; mark rid "$RUN/rid.log"
    say "    holds: $(live_rows | jq -c --arg p "$(panepid rid)" 'select((.pid|tostring) == $p) | {sessionId}')"
    term rid
  done
  ;;
# ---------------------------------------------------------------------------
worktree)
  say "=== where --resume finds a conversation started in a worktree"
  # A worktree left by an earlier run is locked by its (dead) claude: -f -f.
  [ -d "$WS/.claude/worktrees/ddspike10wt" ] && git -C "$WS" worktree remove -f -f "$WS/.claude/worktrees/ddspike10wt"
  git -C "$WS" branch -D worktree-ddspike10wt >/dev/null 2>&1
  pane w1 "$WS" "$RUN/w1.log" "$CLAUDE" -w ddspike10wt --debug-file "$RUN/w1.debug"; sleep 14
  mark w1 "$RUN/w1.log"
  typeline w1 "Remember the word $MARKER. Reply with exactly OK0 and nothing else." 15
  W1=$(live_rows | jq -r --arg p "$(panepid w1)" 'select((.pid|tostring) == $p) | "\(.sessionId) \(.cwd)"')
  WU=${W1%% *}; WCWD=${W1#* }
  say "  worktree session: $WU in $WCWD"
  say "  git worktrees: $(git -C "$WS" worktree list | tr '\n' ';')"
  say "  its transcript: $(ls "$(projdir "$WCWD")"/"$WU".jsonl 2>/dev/null || echo "not under $(projdir "$WCWD")")"
  term w1
  say "--- (a) from the clone's path: claude --resume $WU"
  pane w2 "$WS" "$RUN/w2.log" "$CLAUDE" --resume "$WU"; sleep 14
  mark w2 "$RUN/w2.log"
  say "  its cwd: $(live_rows | jq -r --arg p "$(panepid w2)" 'select((.pid|tostring) == $p) | .cwd')"
  kill -0 "$(panepid w2)" 2>/dev/null && say "  history? $(ask w2)"
  term w2
  say "--- (b) from the clone's path: claude --resume $WU --rc"
  pane w3 "$WS" "$RUN/w3.log" "$CLAUDE" --resume "$WU" --rc ddspike10w; sleep 18
  mark w3 "$RUN/w3.log"
  say "  its cwd: $(live_rows | jq -r --arg p "$(panepid w3)" 'select((.pid|tostring) == $p) | .cwd')  app session: $(rcline "$RUN/w3.log")"
  term w3
  say "--- (c) from the worktree's path: claude --resume $WU"
  pane w4 "$WCWD" "$RUN/w4.log" "$CLAUDE" --resume "$WU"; sleep 14
  mark w4 "$RUN/w4.log"
  say "  its cwd: $(live_rows | jq -r --arg p "$(panepid w4)" 'select((.pid|tostring) == $p) | .cwd')"
  kill -0 "$(panepid w4)" 2>/dev/null && say "  history? $(ask w4)"
  term w4
  say "--- (d) the picker from the clone's path: is the worktree session listed?"
  pane w5 "$WS" "$RUN/w5.log" "$CLAUDE" --resume; sleep 10
  screen w5 | grep -aE '❯|ago|bytes|No sessions|worktree|ddspike10' | head -10 | cut -c1-160 | sed 's/^/    | /'
  term w5
  # --fork-session on a worktree conversation. The binary's resume paths strip
  # the worktree binding when they fork (`cli` prints the strings); where does
  # the fork run? A bare trailing --rc, as the docs sentence has it.
  # wfork <tag>: report the fork a pane holds (cwd, ids, the history).
  # selrow <pane>: the picker's selected row's own metadata line only. A row
  # is two lines, `❯ <title>` then `<age> · <branch> · <size>`, so the branch
  # is on exactly the line after the one `❯` line: nothing else on screen (a
  # neighbouring row, the footer) can match. wfork's descent check then
  # confirms the Enter took the conversation meant.
  selrow() { screen "$1" | grep -v '^\s*$' | grep -a -m1 -A1 '❯' | sed -n 2p; }
  wfork() {
    local p row id; p=$(panepid "$1")
    mark "$1" "$RUN/$1.log"
    row=$(live_rows | jq -c --arg p "$p" 'select((.pid|tostring) == $p) | {sessionId, bridgeSessionId, cwd}')
    id=$(jq -r '.sessionId // empty' <<<"$row")
    say "  its registry row: ${row:-none}  (the worktree is $WCWD, the clone $WS; the original $WU)"
    say "  app session printed: $(rcline "$RUN/$1.log")"
    [ -n "$id" ] && say "  worktree-state records in its transcript: $(cat "$CFG"/projects/*/"$id".jsonl 2>/dev/null | grep -ac '"worktree-state"')"
    # Is it a fork of $WU at all? A fork's records keep the original's uuids,
    # so the first user record of $WU's transcript must be in the fork's; and
    # the answer must be this run's marker. Either failing is a MISMATCH: the
    # row describes some other conversation, and says nothing about $WU.
    local wf ff u0 ans
    wf=$(ls "$CFG"/projects/*/"$WU".jsonl 2>/dev/null | head -1)
    ff=$(ls -t "$CFG"/projects/*/"$id".jsonl 2>/dev/null | head -1)
    u0=$(jq -r 'select(.type == "user") | .uuid' "$wf" 2>/dev/null | head -1)
    ans=""; [ -n "$id" ] && ans=$(ask "$1")
    say "  history? $ans"
    if [ -n "$u0" ] && [ -n "$ff" ] && grep -aqF "\"uuid\":\"$u0\"" "$ff" && printf '%s' "$ans" | grep -qF "$MARKER"; then
      say "  DESCENDS from $WU: its transcript carries $WU's first user record ($u0), and it recalled $MARKER"
    else
      say "  MISMATCH: not shown to be a fork of $WU (first user record ${u0:-?} in its transcript: $(grep -acF "\"uuid\":\"$u0\"" "${ff:-/dev/null}"); marker recalled: $(printf '%s' "$ans" | grep -qF "$MARKER" && echo yes || echo no))"
    fi
  }
  # (i) first, before (g) and (h) add forks to the list. The worktree
  # conversation is then the newest in the repository ((c) just resumed it).
  # Ctrl+W widens the list but keeps the cursor on the row it was on, so on a
  # used scratch (with clone conversations listed first) Enter takes the wrong
  # row; an Up moves it into the search box instead. Enter only when the
  # cursor's row is the worktree's, which on a fresh scratch it is.
  say "--- (i) the picker from the clone's path with --fork-session --rc, Ctrl+W, Enter"
  pane wi "$WS" "$RUN/wi.log" "$CLAUDE" --debug-file "$RUN/wi.debug" --resume --fork-session --rc; sleep 10
  t send-keys -t wi C-w; sleep 3
  say "  after Ctrl+W:"; screen wi | grep -v '^\s*$' | grep -aA1 -E '❯|Search' | head -8 | cut -c1-160 | sed 's/^/    | /'
  if selrow wi | grep -q 'worktree-ddspike10wt'; then
    t send-keys -t wi Enter; sleep 18
    wfork wi
  else
    say "  (i) NOT MEASURED: the cursor is not on the worktree conversation's row (run on a fresh scratch)"
  fi
  term wi
  # (j) the same picker from the worktree's own path, no Ctrl+W: the folder
  # the docs sentence sends a worktree session's operator to. Before (h), the
  # only conversation there is the worktree one.
  say "--- (j) the picker from the worktree's path with --fork-session --rc, Enter"
  pane wj "$WCWD" "$RUN/wj.log" "$CLAUDE" --debug-file "$RUN/wj.debug" --resume --fork-session --rc; sleep 10
  say "  the picker:"; screen wj | grep -v '^\s*$' | grep -aA1 -E '❯|Ctrl\+W' | head -6 | cut -c1-160 | sed 's/^/    | /'
  if selrow wj | grep -q 'worktree-ddspike10wt'; then
    t send-keys -t wj Enter; sleep 18
    wfork wj
  else
    say "  (j) NOT MEASURED: the cursor is not on the worktree conversation's row (run on a fresh scratch)"
  fi
  term wj
  say "--- (g) from the clone's path: claude --resume $WU --fork-session --rc"
  pane wg "$WS" "$RUN/wg.log" "$CLAUDE" --debug-file "$RUN/wg.debug" --resume "$WU" --fork-session --rc; sleep 18
  wfork wg; term wg
  say "--- (h) from the worktree's path: claude --resume $WU --fork-session --rc"
  pane wh "$WCWD" "$RUN/wh.log" "$CLAUDE" --debug-file "$RUN/wh.debug" --resume "$WU" --fork-session --rc; sleep 18
  wfork wh; term wh
  say "--- (e) the reverse: a conversation in the clone, resumed from the worktree"
  pane w6 "$WS" "$RUN/w6.log" "$CLAUDE"; sleep 10
  typeline w6 "Remember the word ${MARKER}X. Reply with exactly OK0 and nothing else." 15
  CU=$(live_rows | jq -r --arg p "$(panepid w6)" 'select((.pid|tostring) == $p) | .sessionId'); term w6
  pane w7 "$WCWD" "$RUN/w7.log" "$CLAUDE" --resume "$CU"; sleep 14
  mark w7 "$RUN/w7.log"
  say "  clone session $CU resumed from the worktree; its cwd: $(live_rows | jq -r --arg p "$(panepid w7)" 'select((.pid|tostring) == $p) | .cwd')"
  kill -0 "$(panepid w7)" 2>/dev/null && say "  history? $(ask w7)"
  say "  its transcript file(s): $(ls "$CFG"/projects/*/"$CU".jsonl | sed 's#.*/projects/##; s#^[^/]*-ws#ws#' | tr '\n' ' ')"
  term w7
  say "--- (f) from a directory outside the repository (\$HOME): both conversations"
  for id in "$WU" "$CU"; do
    pane w8 "$SHOME" "$RUN/w8.log" "$CLAUDE" --resume "$id"; sleep 14
    say "  claude --resume $id from $SHOME"; mark w8 "$RUN/w8.log"
    say "    holds: $(live_rows | jq -c --arg p "$(panepid w8)" 'select((.pid|tostring) == $p) | {sessionId, cwd}')"
    term w8
  done
  say "--- cleanup: the worktree"
  # claude -w locks the worktree with its pid; it is ours and gone, so -f -f.
  git -C "$WS" worktree remove -f -f "$WCWD" 2>&1 | sed 's/^/    /'
  git -C "$WS" branch -D worktree-ddspike10wt 2>&1 | sed 's/^/    /'
  ;;
esac
say "=== done ($MODE)"
