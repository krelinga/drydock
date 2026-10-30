# Spike 06 — shared helpers. Sourced, not run.
#
# Every claude this harness starts runs under `env -i` with only what a
# Drydock container gives it: HOME (a scratch one), PATH, TERM, LANG, CLAUDE_CONFIG_DIR and
# DISABLE_AUTOUPDATER=1. That matters twice over: tmux panes inherit the tmux
# *server's* environment (Spike 02's first trap), and a harness run from inside
# a Claude Code session would otherwise hand every child CLAUDECODE=1,
# CLAUDE_CODE_SESSION_ID and friends, which change how a nested claude behaves.
#
# All panes live on a private tmux server (`tmux -L dd-spike-06`), so `reap`
# can kill exactly what this harness started.

S="${S:?set S to the scratch dir setup.sh built}"
WS="${WS:-$S/ws}"
CFG="${CFG:-$S/cfg}"
RUN="${RUN:-$S/run}"
# A throwaway HOME as well: remote-control writes each spawned session's
# system-prompt file under $HOME/.claude/bridge-spawn, *not* under
# CLAUDE_CONFIG_DIR, so the real HOME would otherwise be written to.
SHOME="${SHOME:-$S/home}"
CLAUDE="${CLAUDE_BIN:-$(command -v claude)}"
TMUX_L=dd-spike-06
mkdir -p "$RUN" "$SHOME"

say() { printf '%s\n' "$*"; }
t() { tmux -L "$TMUX_L" "$@"; }

# The token guard. $CFG/.credentials.json is a copy of the owner's live
# refresh token, and the copy refreshes on its own: the refresh lock is per
# CLAUDE_CONFIG_DIR, so nothing coordinates it with the real config. A refresh
# rotates the token (Spike 00), and the real config's next refresh then finds
# its token dead and blanks the login. Claude Code refreshes only near the
# access token's expiresAt (the --bg daemon schedules it a few minutes before),
# so refuse to run unless expiresAt is TOKEN_MARGIN_MIN (default 60) minutes
# past now -- far more than any mode takes.
token_guard() { # token_guard <credentials.json>
  local f="$1" exp left
  exp=$(jq -r '.claudeAiOauth.expiresAt // 0' "$f" 2>/dev/null) || exp=0
  left=$(( (${exp:-0} / 1000 - $(date +%s)) / 60 ))
  if [ "$left" -lt "${TOKEN_MARGIN_MIN:-60}" ]; then
    say "REFUSING: $f's access token expires in ${left} min (< ${TOKEN_MARGIN_MIN:-60})."
    say "  A scratch copy could refresh it and rotate the owner's real login dead."
    say "  Wait for the real config to refresh (run any claude command normally), then"
    say "  re-run ./setup.sh to take a fresh copy."
    exit 3
  fi
}

# One-shot commands (help text, --version): no TTY, stdin closed, bounded.
cenv() {
  timeout 30 env -i HOME="$SHOME" PATH="$PATH" TERM=dumb LANG="${LANG:-C.UTF-8}" \
    CLAUDE_CONFIG_DIR="$CFG" DISABLE_AUTOUPDATER=1 "$@" </dev/null
}

# Strip CSI sequences but keep OSC 8 payloads (where session URLs live).
clean() { sed 's/\x1b\[[0-9;?]*[A-Za-z]//g' "$1" | tr -d '\r'; }
envid() { grep -aoE 'env_[A-Za-z0-9]+' "$1" | head -1; }
# Ids only from OSC 8 targets, as Drydock does.
sessids() { grep -aoE $'\e\\]8;;https://claude\\.ai/code/session_[A-Za-z0-9]+' "$1" \
  | grep -aoE 'session_[A-Za-z0-9]+' | sort -u | tr '\n' ' '; }
cap() { clean "$1" | grep -aoE 'Capacity: [0-9]+/[0-9]+' | tail -1; }

# pane <name> <dir> <log> <cmd...>: start cmd in a new detached 200x50 pane,
# exec'd so the pane pid is the process, output appended to log.
pane() {
  local name="$1" dir="$2" log="$3"; shift 3
  : > "$log"
  t kill-session -t "$name" 2>/dev/null
  local q; q=$(printf '%q ' "$@")
  t new-session -d -s "$name" -c "$dir" -x 200 -y 50 \
    "exec env -i HOME='$SHOME' PATH='$PATH' TERM=xterm-256color LANG='${LANG:-C.UTF-8}' \
       CLAUDE_CONFIG_DIR='$CFG' DISABLE_AUTOUPDATER=1 $q"
  t pipe-pane -o -t "$name" "cat >> '$log'"
}
panepid() { t list-panes -t "$1" -F '#{pane_pid}' 2>/dev/null | head -1; }
screen() { t capture-pane -p -t "$1" 2>/dev/null; }

# waitfor <seconds> <file> <ERE>: poll the cleaned log for a pattern.
waitfor() {
  local n="$1" f="$2" re="$3" i
  for i in $(seq 1 "$n"); do
    clean "$f" | grep -aqE "$re" && return 0
    sleep 1
  done
  return 1
}
# waitscreen <seconds> <pane> <ERE>: poll the rendered screen instead.
waitscreen() {
  local n="$1" p="$2" re="$3" i
  for i in $(seq 1 "$n"); do
    screen "$p" | grep -aqE "$re" && return 0
    sleep 1
  done
  return 1
}

# The server, exactly as design §8 runs it (less the secrets prelude).
RC_ARGS=(remote-control --verbose --spawn worktree --capacity 4
         --remote-control-session-name-prefix ddspike06)

start_server() { # <log> [dir]
  pane srv "${2:-$WS}" "$1" "$CLAUDE" "${RC_ARGS[@]}" --debug-file "$1.debug"
  waitfor 60 "$1" 'Capacity: [1-9]' || { say "server did not reach a live session:"; clean "$1" | tail -5; return 1; }
}

# SIGTERM a pane's process and wait for it to go; SIGKILL only after 20 s.
term() {
  local p="$1" pid i; pid=$(panepid "$p")
  [ -n "$pid" ] || return 0
  kill -TERM "$pid" 2>/dev/null
  for i in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || return 0; sleep 1; done
  say "  $p ($pid) ignored SIGTERM for 20 s; SIGKILL"
  kill -KILL "$pid" 2>/dev/null
}

# Reap everything: SIGTERM each pane's process (the server last, so a session
# it serves is not orphaned first), then this harness's tmux server, then any
# background session (`bg` mode) still running on this config; Claude Code's
# daemon exits by itself 5 s after its last session ends.
reap() {
  local s id
  for s in $(t list-sessions -F '#{session_name}' 2>/dev/null | grep -v '^srv$'); do term "$s"; done
  term srv
  t kill-server 2>/dev/null
  # Only the background sessions this harness started: `bg` records each id
  # it is handed in $BGIDS (outside the per-mode directory, so a run that died
  # is still cleaned up by the next). Never "everything claude agents lists":
  # the daemon's sockets are keyed by uid, so that list may hold the owner's.
  if [ -f "$BGIDS" ]; then
    # An id whose stop failed stays listed, for the next run's reap to retry.
    : > "$BGIDS.keep"
    while read -r id; do
      [ -n "$id" ] || continue
      (cd "$WS" && cenv "$CLAUDE" stop "$id") >/dev/null 2>&1 || printf '%s\n' "$id" >> "$BGIDS.keep"
    done < "$BGIDS"
    if [ -s "$BGIDS.keep" ]; then mv "$BGIDS.keep" "$BGIDS"; else rm -f "$BGIDS" "$BGIDS.keep"; fi
  fi
  return 0
}
BGIDS="$S/bg.ids"

# is_ours <pid> <ERE>: the pid's command line matches and its environment
# carries this harness's CLAUDE_CONFIG_DIR. Checked before any signal to a pid
# the harness did not start itself (the --bg daemon).
is_ours() {
  [ -r "/proc/$1/cmdline" ] || return 1
  tr '\0' ' ' < "/proc/$1/cmdline" | grep -qE "$2" || return 1
  tr '\0' '\n' < "/proc/$1/environ" 2>/dev/null | grep -qx "CLAUDE_CONFIG_DIR=$CFG"
}

# The transcript directory Claude Code uses for a working directory.
projdir() { printf '%s/projects/%s' "$CFG" "$(printf '%s' "$1" | sed 's/[^A-Za-z0-9]/-/g')"; }

# typeline <pane> <text> [wait]: type a line and press Enter. A trailing space
# after a slash command closes the autocomplete menu, which otherwise turns
# Enter into "pick the highlighted command" (`/remote-control` + Enter ran
# `/remote-env` in this spike's first attempt).
typeline() {
  t send-keys -t "$1" -l "$2"; sleep 1
  t send-keys -t "$1" Enter; sleep "${3:-3}"
}

# Claude Code's live-session registry: $CFG/sessions/<pid>.json, one per
# process, naming its local session id and, when it holds Remote Control, the
# bridge session id. `registry` prints one compact line per live process.
# A process killed with SIGKILL leaves its row behind, so each row says
# whether its pid is still alive.
registry() {
  local f pid
  for f in "$CFG"/sessions/*.json; do
    [ -f "$f" ] || continue
    pid=$(jq -r .pid "$f")
    jq -c --arg alive "$(kill -0 "$pid" 2>/dev/null && echo yes || echo DEAD)" \
      '{pid, alive: $alive, sessionId, bridgeSessionId, entrypoint, status}' "$f"
  done
}
# Local session id of the process holding a bridge session. The row can land a
# few seconds after the session is announced, so wait up to 20 s for it.
uuid_of() {
  local i u
  for i in $(seq 1 20); do
    u=$(live_rows | jq -rs --arg b "$1" 'map(select(.bridgeSessionId == $b)) | sort_by(.startedAt) | last | .sessionId // empty')
    [ -n "$u" ] && { printf '%s\n' "$u"; return 0; }
    sleep 1
  done
  return 1
}
# The rows of live processes only.
live_rows() {
  local f
  for f in "$CFG"/sessions/*.json; do
    [ -f "$f" ] && kill -0 "$(jq -r .pid "$f")" 2>/dev/null && cat "$f"
  done
}
# Every live row naming a local session id (how many processes hold it).
holders() { live_rows | jq -c --arg s "$1" 'select(.sessionId == $s) | {pid, bridgeSessionId, entrypoint}'; }

# The session URL an interactive Remote Control prints, in plain text (it is
# *not* wrapped in OSC 8, unlike the server's).
rcline() { clean "$1" | grep -aoE 'claude\.ai/code/session_[A-Za-z0-9]+' | tail -1 | grep -aoE 'session_[A-Za-z0-9]+'; }

TREE() { node "$(dirname "${BASH_SOURCE[0]}")/tree.mjs" "$@"; }
