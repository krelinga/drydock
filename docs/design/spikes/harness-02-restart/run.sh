#!/usr/bin/env bash
# Spike 02 — does a `claude remote-control` server survive a supervisor restart?
#
# Usage: ./run.sh [sigkill|sigterm|both] [dir]
#
# Starts a Remote Control server in `dir` under tmux, waits for it to have a
# live session (not just to be listening — the distinction turned out to matter),
# records the environment and session identity it advertises, signals it the way
# Drydock's supervisor would (SIGKILL = Drydock crashed; SIGTERM = Drydock
# stopped it on purpose), then restarts in the same directory and compares what
# comes back.
#
# `dir` must already be trusted and the Remote Control consent dialog answered.
# ./seed-config.sh builds a CLAUDE_CONFIG_DIR that satisfies every gate without
# a human; point CLAUDE_CONFIG_DIR at the result. See ../02-rc-restart.md.
set -uo pipefail

MODE="${1:-both}"
DIR="${2:-$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)}"
CLAUDE="${CLAUDE_BIN:-$(command -v claude)}"
RUN="${DRYDOCK_SPIKE_RUN:-$(mktemp -d -t drydock-spike-02-XXXXXX)}"
SESSION=dd-spike-02
MAX_WAIT="${MAX_WAIT:-300}"
# Seconds to wait before a round, so a registration left by the previous round
# has lapsed and the window measured belongs to this round's kill.
SETTLE="${SETTLE:-120}"

RC_ARGS=(remote-control --verbose --spawn worktree --capacity 4
         --remote-control-session-name-prefix drydockspike)
# `nosession` removes the pre-created session, which is what exposes the 409.
[ "$MODE" = nosession ] && RC_ARGS+=(--no-create-session-in-dir)

say() { printf '%s\n' "$*"; }
# Strip ANSI SGR but keep OSC 8 hyperlink payloads, which is where the
# per-session URL lives.
clean() { sed 's/\x1b\[[0-9;]*[A-Za-z]//g' "$1" | tr -d '\r'; }
envid()  { grep -aoE 'env_[A-Za-z0-9]+' "$1" | head -1; }
sessid() { grep -aoE 'session_[A-Za-z0-9]+' "$1" | sort -u | tr '\n' ' '; }
cap()    { clean "$1" | grep -aoE 'Capacity: [0-9]+/[0-9]+' | tail -1; }

reap() {
  tmux kill-session -t "$SESSION" 2>/dev/null
  # The bracket keeps the pattern from matching this script's own command line,
  # which otherwise makes pkill kill the shell running it.
  pkill -f 'remote-control --verbose --spawn worktre[e]' 2>/dev/null
  sleep 2
}

# Start a server and wait until it reports a live session, so the restart test
# exercises the case where there is something to preserve. Returns non-zero if
# the start was refused.
start_server() {
  local log="$1"
  : > "$log"
  tmux kill-session -t "$SESSION" 2>/dev/null
  # `exec` so the tmux pane pid *is* the claude process.
  # tmux panes inherit the (possibly long-running) tmux *server's* environment,
  # not this shell's, so CLAUDE_CONFIG_DIR has to be passed explicitly or the
  # server silently reads the wrong config and reports the workspace untrusted.
  tmux new-session -d -s "$SESSION" -c "$DIR" -x 200 -y 50 \
    "exec env CLAUDE_CONFIG_DIR='${CLAUDE_CONFIG_DIR:-$HOME/.claude}' \
       '$CLAUDE' ${RC_ARGS[*]} --debug-file '$log.debug'"
  tmux pipe-pane -o -t "$SESSION" "cat >> '$log'"
  local i
  for i in $(seq 1 30); do
    if [ "$MODE" = nosession ]; then
      # No pre-created session by design, so "listening" is as far as it goes.
      grep -aq 'Environment ID' "$log" && return 0
    else
      clean "$log" | grep -aqE 'Capacity: [1-9]' && return 0
    fi
    grep -aqE '409|Error:' "$log" && return 1
    sleep 2
  done
  grep -aq 'Environment ID' "$log" && return 0
  return 1
}

server_pid() { tmux list-panes -t "$SESSION" -F '#{pane_pid}' 2>/dev/null | head -1; }

one_round() {
  local sig="$1"
  local log1="$RUN/$sig-before.log" log2="$RUN/$sig-after.log"
  local signame env1 env2 pid t0 el att=0
  case "$sig" in
    sigkill|nosession) signame=KILL ;;
    sigterm) signame=TERM ;;
    *) say "unknown mode: $sig (use sigterm, sigkill, nosession, or both)"; return 1 ;;
  esac

  say "=== $sig ==="
  reap
  say "  settling ${SETTLE}s so any previous registration has lapsed"
  sleep "$SETTLE"
  if ! start_server "$log1"; then
    say "  could not start: $(clean "$log1" | grep -aoE '(Error|409)[^|]*' | head -1 | cut -c1-140)"
    say "  (a 409 here means a previous run's registration has not lapsed; retry shortly)"
    return 1
  fi
  env1=$(envid "$log1"); pid=$(server_pid)
  say "  before: env=$env1 pid=$pid  $(cap "$log1")"
  say "          sessions=$(sessid "$log1")"

  say "  sending SIG$signame to $pid"
  kill "-$signame" "$pid" 2>/dev/null
  sleep 4
  kill -0 "$pid" 2>/dev/null && say "  WARNING: pid $pid still alive" || say "  process gone"
  say "  shutdown notice: $(clean "$log1" | grep -aoE 'Environment preserved[^·]*|Shutting down [0-9]+ active session[^…]*' | tail -2 | tr '\n' ' ')"

  say "  restarting in the same directory:"
  t0=$(date +%s)
  while :; do
    att=$((att+1))
    if start_server "$log2"; then
      el=$(( $(date +%s) - t0 ))
      env2=$(envid "$log2")
      say "    t=${el}s attempt $att: ACCEPTED"
      say "    after:  env=$env2  $(cap "$log2")"
      say "            sessions=$(sessid "$log2")"
      if [ "$env1" = "$env2" ]; then
        say "    => environment PRESERVED across $sig"
      else
        say "    => environment CHANGED across $sig ($env1 -> $env2)"
      fi
      break
    fi
    el=$(( $(date +%s) - t0 ))
    if clean "$log2" | grep -q 409; then
      say "    t=${el}s attempt $att: refused 409 (folder still registered)"
    else
      say "    t=${el}s attempt $att: $(clean "$log2" | grep -aoE 'Error:.*' | head -1 | cut -c1-110)"
    fi
    if [ "$el" -ge "$MAX_WAIT" ]; then say "    gave up after ${el}s"; break; fi
    sleep 5
  done
  reap
  say ""
}

say "claude under test: $("$CLAUDE" --version 2>&1 | head -1)"
say "dir=$DIR   config=${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
say "artifacts=$RUN"
say ""

case "$MODE" in
  both) one_round sigterm; one_round sigkill ;;
  *)    one_round "$MODE" ;;
esac

say "=== output patterns worth scraping (from this run) ==="
for f in "$RUN"/*.log; do clean "$f"; done 2>/dev/null \
  | grep -ahoE 'Environment ID: env_[A-Za-z0-9]+|claude\.ai/code\?environment=env_[A-Za-z0-9]+|claude\.ai/code/session_[A-Za-z0-9]+[^ ]*|Capacity: [0-9]+/[0-9]+|Error: [^ ].*' \
  | sort -u | sed 's/^/  /'
