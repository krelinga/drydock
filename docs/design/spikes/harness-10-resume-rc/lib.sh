# Spike 10 — shared helpers. Sourced, not run.
#
# Spike 06's lib.sh is the base, sourced as is: the `env -i` panes on a private
# tmux server, the fail-closed token guard (`token_guard`, with `is_uint` before
# any arithmetic), the waits, the OSC-8-only id scrape, the live-session
# registry readers, `term` (SIGTERM, SIGKILL only after 20 s) and `reap`. What
# changes here is only the name of things, so this harness never touches Spike
# 06's tmux server or anything else:
#
#   TMUX_L   dd-spike-10 (Spike 06 used dd-spike-06; the default server and an
#            operator's `claude` session are never touched)
#   RC_ARGS  the server exactly as design §8 runs it, sessions named ddspike10-…
#
# Nothing here starts a container; every process is on the host under `env -i`
# with only a Drydock container's variables, as Spike 06 ran.

HERE10="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$HERE10/../harness-06-attach/lib.sh"
TMUX_L=dd-spike-10
RC_ARGS=(remote-control --verbose --spawn worktree --capacity 4
         --remote-control-session-name-prefix ddspike10)
# Spike 06's reap also stops --bg ids listed here. This harness starts none;
# point it at a file that is never written.
BGIDS="$S/spike10-no-bg.ids"
TREE() { node "$HERE10/../harness-06-attach/tree.mjs" "$@"; }

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
  t0=$(now)
  typeline "$1" 'What word did I ask you to remember? Answer with that word only.' 18
  ans=$(jq -r --arg t "$t0" 'select(.type == "assistant" and (.timestamp // "") >= $t)
        | .message.content | if type == "array" then map(.text // empty) | join("") else . end' "$f" 2>/dev/null \
        | grep -v '^$' | tail -1)
  printf '%s (in %s, file %s)' "${ans:-NO NEW ANSWER}" "${sid:0:8}" "$(basename "$(dirname "${f:-?/?}")" | sed 's/.*-ws/ws/')"
}

# bridge <debug-file>: the bridge's own account of what it did (minted a
# session, reattached, uploaded history, was refused). Heartbeats and queue
# chatter dropped.
bridge() {
  [ -f "$1" ] || { say "    !! no debug file $1"; return 0; }
  grep -aE '\[(bridge(:[a-z]+)?|remote-bridge)\]' "$1" \
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
