# Spike 10 — shared helpers. Sourced, not run.
#
# Spike 06's lib.sh is the base: the `env -i` panes on a private tmux server,
# `is_uint`, the waits, the OSC-8-only id scrape, the live-session registry
# readers and `term` (SIGTERM, SIGKILL only after 20 s). Renamed, so this
# harness never touches Spike 06's tmux server or anything else:
#
#   TMUX_L   dd-spike-10 (Spike 06 used dd-spike-06; the default server and an
#            operator's `claude` session are never touched)
#   RC_ARGS  the server exactly as design §8 runs it, sessions named ddspike10-…
#
# Replaced or added: a refusal of a scratch that is the real config or $HOME
# (before Spike 06's lib creates anything), Spike 08's token guard (it counts
# the mode's minutes), and `reap`, which ends with a sweep of every process
# still carrying CLAUDE_CONFIG_DIR=$CFG.
#
# Nothing here starts a container; every process is on the host under `env -i`
# with only a Drydock container's variables, as Spike 06 ran.

HERE10="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The scratch must be a scratch (Spike 08's refusal, widened): run.sh deletes
# under $S, every process writes $CFG, and remote-control writes under $SHOME.
# Refuse, before anything is created or deleted, a CFG, S or SHOME that is, is
# inside, or contains the real config (~/.claude, and $CLAUDE_CONFIG_DIR when
# set), and an S or SHOME that is $HOME. Paths are compared resolved
# (readlink -m), so a symlink or a `..` does not get past it.
: "${S:?set S to the scratch dir setup.sh built}"
CFG="${CFG:-$S/cfg}"; SHOME="${SHOME:-$S/home}"
_rp() { readlink -m -- "$1"; }
_overlaps() { # _overlaps <a> <b>: a is b, or is inside it, or contains it
  local a b; a=$(_rp "$1"); b=$(_rp "$2")
  [ "$a" = "$b" ] && return 0
  case "$a/" in "$b"/*) return 0 ;; esac
  case "$b/" in "$a"/*) return 0 ;; esac
  return 1
}
for _real in "$HOME/.claude" ${CLAUDE_CONFIG_DIR:+"$CLAUDE_CONFIG_DIR"}; do
  for _p in "$CFG" "$S" "$SHOME"; do
    _overlaps "$_p" "$_real" && { printf 'REFUSING: %s overlaps the real config %s; S, CFG and SHOME must be a scratch copy\n' "$_p" "$_real"; exit 2; }
  done
done
for _p in "$S" "$SHOME"; do
  [ "$(_rp "$_p")" = "$(_rp "$HOME")" ] && { printf 'REFUSING: %s is $HOME, not a scratch dir\n' "$_p"; exit 2; }
done
unset _real _p

. "$HERE10/../harness-06-attach/lib.sh"
TMUX_L=dd-spike-10
RC_ARGS=(remote-control --verbose --spawn worktree --capacity 4
         --remote-control-session-name-prefix ddspike10)
# Spike 06's reap also stops --bg ids listed here. This harness starts none;
# point it at a file that is never written.
BGIDS="$S/spike10-no-bg.ids"
TREE() { node "$HERE10/../harness-06-attach/tree.mjs" "$@"; }

# The token guard, Spike 08's: the copied access token's expiresAt must be
# TOKEN_MARGIN_MIN (default 60, floor 30) minutes away *after* the mode has run
# for the minutes it says it needs (run.sh passes MODE_MIN, which counts
# WAIT_APP). It replaces Spike 06's, which ignored the mode's length. Fails
# closed: every value passes is_uint (Spike 06's, an explicit digit list)
# before any arithmetic, and every check is `[ ok ] || refuse`.
token_guard() { # token_guard <credentials.json> <minutes the mode runs>
  local f="$1" need="${2:-}" margin="${TOKEN_MARGIN_MIN:-60}" exp left want now
  is_uint "$need" || { say "REFUSING: the mode's minutes ('$need') are not a whole number (no leading zero, at most 15 digits)"; exit 3; }
  is_uint "$margin" || { say "REFUSING: TOKEN_MARGIN_MIN='$margin' is not a whole number of minutes (no leading zero, at most 15 digits)"; exit 3; }
  [ "$margin" -ge 30 ] || { say "REFUSING: TOKEN_MARGIN_MIN=$margin is below the floor of 30"; exit 3; }
  exp=$(jq -r '.claudeAiOauth.expiresAt' "$f" 2>/dev/null) || { say "REFUSING: cannot read expiresAt from $f"; exit 3; }
  is_uint "$exp" || { say "REFUSING: $f's expiresAt ('$exp') is not a whole number of milliseconds (no leading zero, at most 15 digits)"; exit 3; }
  now=$(date +%s) && is_uint "$now" || { say "REFUSING: the clock ('$now') is not a whole number of seconds (no leading zero, at most 15 digits)"; exit 3; }
  left=$(( (exp / 1000 - now) / 60 ))
  want=$(( margin + need ))
  [ "$left" -ge "$want" ] || {
    say "REFUSING: $f's access token expires in ${left} min (< ${margin} margin + ${need} the mode runs)."
    say "  A scratch copy could refresh it and rotate the owner's real login dead."
    say "  Let the real config refresh, delete \$S, and re-run ./setup.sh."
    exit 3
  }
  say "token guard: access token expires in $left min (needs $want: margin $margin + mode $need)"
}

# Every process holding the copied token: any pid whose environment carries
# CLAUDE_CONFIG_DIR=$CFG (Spike 06's is_ours, any command line). The panes are
# gone by the time this runs, so what it finds outlived them (a server's
# session child, a tool a session ran). SIGTERM, up to 20 s, then SIGKILL; each
# pid is printed. Returns 1 if any survives the SIGKILL.
sweep_cfg() {
  local p i left=() alive bad=0
  for p in /proc/[0-9]*; do
    p=${p#/proc/}
    { [ "$p" = "$$" ] || [ "$p" = "$BASHPID" ]; } && continue
    [ -r "/proc/$p/environ" ] && is_ours "$p" . && left+=("$p")
  done
  [ "${#left[@]}" -gt 0 ] || { say "  sweep: no process carries CLAUDE_CONFIG_DIR=$CFG"; return 0; }
  for p in "${left[@]}"; do
    say "  sweep: SIGTERM $p ($(tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null | cut -c1-80))"
    kill -TERM "$p" 2>/dev/null
  done
  for i in $(seq 1 20); do
    alive=()
    for p in "${left[@]}"; do is_ours "$p" . && alive+=("$p"); done
    left=("${alive[@]}")
    [ "${#left[@]}" -gt 0 ] || { say "  sweep: all gone after SIGTERM"; return 0; }
    sleep 1
  done
  for p in "${left[@]}"; do say "  sweep: $p ignored SIGTERM for 20 s; SIGKILL"; kill -KILL "$p" 2>/dev/null; done
  sleep 2
  for p in "${left[@]}"; do is_ours "$p" . && { say "  sweep: $p SURVIVED SIGKILL"; bad=1; }; done
  return "$bad"
}

# reap: Spike 06's (each pane SIGTERMed, the server last, then the private tmux
# server), then the sweep. Signals are held off while it runs, so a Ctrl-C or a
# timeout's TERM cannot start a second reap inside the first; the caller's
# traps are put back afterwards. Returns the sweep's status.
eval "reap06() $(declare -f reap | tail -n +2)"
reap() {
  local saved rc; saved=$(trap -p INT TERM HUP)
  trap '' INT TERM HUP
  reap06; sweep_cfg; rc=$?
  trap - INT TERM HUP; eval "$saved"
  return "$rc"
}

# A marker word the conversation is told to remember before the hand-off. The
# hand-off's terminal is then asked for it: the answer is the history test.
MARKER="${MARKER:-QUOKKA$(od -An -N2 -tu2 /dev/urandom | tr -d ' ')}"

# ask <pane>: ask for the marker; print the answer, read from the transcript
# of the conversation the pane's process holds (its registry row names it),
# and only an assistant record written after the question. The screen is not
# evidence: a resumed session redraws its history, so an earlier answer can be
# the last `●` line on screen while the question went nowhere (a dialog ate
# it, in this spike's first run).
ask() {
  local row sid f t0 ans
  row=$(live_rows | jq -c --arg p "$(panepid "$1")" 'select((.pid|tostring) == $p)' | head -1)
  sid=$(jq -r '.sessionId // empty' <<<"$row")
  [ -n "$sid" ] || { printf 'NO REGISTRY ROW for pane %s' "$1"; return 0; }
  # Not projdir "$cwd": a conversation resumed from another directory keeps
  # writing the file it started in. The newest file of that id, anywhere.
  f=$(ls -t "$CFG"/projects/*/"$sid".jsonl 2>/dev/null | head -1)
  # now() has one-second resolution: wait out the second the question might
  # share with an earlier answer, so `>= t0` cannot pick that answer up.
  t0=$(now); sleep 1
  typeline "$1" 'What word did I ask you to remember? Answer with that word only.' 18
  ans=$(jq -r --arg t "$t0" 'select(.type == "assistant" and (.timestamp // "") >= $t)
        | .message.content | if type == "array" then map(.text // empty) | join("") else . end' "$f" 2>/dev/null \
        | grep -v '^$' | tail -1)
  printf '%s (in %s, file %s)' "${ans:-NO NEW ANSWER}" "${sid:0:8}" "$(basename "$(dirname "${f:-?/?}")" | sed 's/.*-ws/ws/')"
}

# bridge <debug-file>: the bridge's own account of what it did (minted a
# session, reattached, uploaded history, was refused), and the persistence
# sync's (`Refusing backfill: …`, history not uploaded). Heartbeats and queue
# chatter dropped.
bridge() {
  [ -f "$1" ] || { say "    !! no debug file $1"; return 0; }
  grep -aE '\[(bridge(:[a-z]+)?|remote-bridge|persistence-sync)\]' "$1" \
    | grep -avE 'Heartbeat|Queued|Drained|poll interval' | cut -c26-230 | head -"${2:-14}" | sed 's/^/    > /'
}

# The last few lines Spike 06's `mark` would have printed, for a pane.
MARKERS='No conversation found|Remote Control not started here|already has Remote Control|own copy of the session|remote-control is active|/rc active|standing down|code 4090|already served|already in use|running in another terminal|running in the background|fork-session|Error:|Remote Control (failed|disconnected)|organization|trust this folder|Explicit enable|No sessions match'
mark() {
  { screen "$1"; clean "$2"; } | grep -aoE "[^│]{0,40}($MARKERS)[^│]{0,110}" | sed 's/  */ /g' | sort -u | sed 's/^/    | /' | head -12
}
turns() { # turns <transcript>: user/assistant record count
  [ -f "$1" ] || { echo 0; return; }
  jq -s 'map(select(.type == "user" or .type == "assistant")) | length' "$1" 2>/dev/null || echo "?"
}
now() { date -u +%Y-%m-%dT%H:%M:%S; }
# gained <debug-file> <iso-time>: lines a debug file gained since a moment,
# and every one that is not a heartbeat (Spike 06's dbg_gained).
gained() {
  [ -f "$1" ] || { say "    !! no debug file $1: cannot say what the process saw"; return 0; }
  local n o
  n=$(awk -v t="$2" '$1 ~ /^[0-9][0-9][0-9][0-9]-/ && $1 >= t' "$1" | wc -l)
  o=$(awk -v t="$2" '$1 ~ /^[0-9][0-9][0-9][0-9]-/ && $1 >= t' "$1" | grep -avE 'Heartbeat sent|switching poll interval' | cut -c26-180)
  say "    debug file gained $n lines since $2, $(printf '%s' "$o" | grep -c .) other than heartbeats:"
  [ -n "$o" ] && printf '%s\n' "$o" | tail -8 | sed 's/^/    > /'
  return 0
}
# The server's environment link and its live sessions, from its tail.
srvstate() { say "  server: env=$(envid "$1") sessions(OSC 8)=[$(sessids "$1")] $(screen srv | grep -aoE 'Capacity: [0-9]+/[0-9]+' | tail -1) alive=$(kill -0 "$(panepid srv)" 2>/dev/null && echo yes || echo no)"; }
