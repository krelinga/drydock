# Spike 08 — shared helpers. Sourced, not run.
#
# Each "workspace" is a scratch container (dd-s08-<w>, image dd-spike-08) run
# the way `devcontainer up` runs one when it overrides the command: PID 1 is
# the CLI's `sh` sleep loop, which exits on SIGTERM. The container gets only
# what the Feature's containerEnv gives Claude Code (CLAUDE_CONFIG_DIR,
# DISABLE_AUTOUPDATER=1, CLAUDE_ENV_FILE) plus HOME, and every command is a
# `docker exec` (what `devcontainer exec` is), so no variable of the shell
# running this harness — a Claude Code session's CLAUDECODE=1 among them —
# reaches anything inside.
#
# The pinned claude is the host's binary, bind-mounted read-only at
# /usr/local/bin/claude, so the version under test is the one `run.sh` prints.
#
# Cleanup is by recorded name only: `ctr_up` appends each container it starts to
# $S/ctr.names and each background id it is handed goes to $S/bg.ids;
# `reap` stops those ids (`claude stop`, the graceful part), then docker stops
# and removes those containers, and nothing else. Every container also carries
# the label dd-spike-08=1, by which sweep.sh removes what a killed run left
# (run.sh and setup.sh run it first). No process outside these containers is
# ever signalled, and no tmux server but this harness's private one is touched.

S="${S:?set S to the scratch dir setup.sh built}"
CFG="${CFG:-$S/cfg}"
RUN="${RUN:-$S/run}"
CLAUDE="${CLAUDE_BIN:-$(readlink -f "$(command -v claude)")}"
TMUX_L=dd-spike-08
CTRS="$S/ctr.names"
BGIDS="$S/bg.ids"
IN_CFG=/home/vscode/.claude

# The scratch config must be the scratch config: fresh_state deletes in it and
# every container mounts it read-write, so refuse one that resolves to the real
# config, or a scratch dir that is $HOME (S=$HOME CFG=$HOME/.claude).
_rp() { readlink -m -- "$1"; }
if [ "$(_rp "$CFG")" = "$(_rp "$HOME/.claude")" ] || [ "$(_rp "$S")" = "$(_rp "$HOME")" ] \
   || { [ -n "${CLAUDE_CONFIG_DIR:-}" ] && [ "$(_rp "$CFG")" = "$(_rp "$CLAUDE_CONFIG_DIR")" ]; }; then
  printf 'REFUSING: CFG=%s / S=%s is the real config or $HOME, not a scratch copy\n' "$CFG" "$S"; exit 2
fi
mkdir -p "$RUN"

say() { printf '%s\n' "$*"; }
t() { tmux -L "$TMUX_L" "$@"; }
now() { date -u +%Y-%m-%dT%H:%M:%S; }

# The token guard (Spike 06's, plus the mode's own length): refuse unless the
# copied access token's expiresAt is still TOKEN_MARGIN_MIN (default 60)
# minutes away once the mode has run for the minutes it says it needs. The
# --bg daemon schedules a proactive refresh four minutes before expiresAt
# (result 10); a refresh of this copy rotates the owner's real refresh token
# dead. It covers only that scheduled refresh: a 401 on the copy's access
# token (revoked server-side, say) would send Claude Code to refresh it at
# once, the residual Spike 06 has too.
token_guard() { # token_guard <credentials.json> [minutes the mode runs]
  # Fails closed: every value is checked to be digits before any arithmetic
  # (a bash arithmetic error aborts that line and carries on), and every
  # step refuses with an explicit `|| exit`.
  local f="$1" need="${2:-0}" margin="${TOKEN_MARGIN_MIN:-60}" exp left want now
  case "$need" in ''|*[!0-9]*) say "REFUSING: the mode's minutes ('$need') are not a whole number"; exit 3 ;; esac
  case "$margin" in ''|*[!0-9]*) say "REFUSING: TOKEN_MARGIN_MIN='$margin' is not a whole number of minutes"; exit 3 ;; esac
  [ "$margin" -ge 30 ] || { say "REFUSING: TOKEN_MARGIN_MIN=$margin is below the floor of 30"; exit 3; }
  exp=$(jq -r '.claudeAiOauth.expiresAt' "$f" 2>/dev/null) || { say "REFUSING: cannot read expiresAt from $f"; exit 3; }
  case "$exp" in ''|*[!0-9]*) say "REFUSING: $f's expiresAt ('$exp') is not a whole number of milliseconds"; exit 3 ;; esac
  now=$(date +%s) || exit 3
  left=$(( (exp / 1000 - now) / 60 )) || exit 3
  want=$(( margin + need )) || exit 3
  if [ "$left" -lt "$want" ]; then
    say "REFUSING: $f's access token expires in ${left} min (< ${margin} margin + ${need} the mode runs)."
    say "  A scratch copy could refresh it and rotate the owner's real login dead."
    say "  Let the real config refresh, delete \$S, and re-run ./setup.sh."
    exit 3
  fi
  say "token guard: access token expires in $left min (needs $want: margin $margin + mode $need)"
}

ctr() { printf 'dd-s08-%s' "$1"; }
# ctr_up <w> [cfg-dir] [extra docker run args...]: start workspace container <w>
# on the shared config (or another config dir), $(wsdir <w>) = $S/ws-<w>.
# wsdir <w>: where the clone is mounted. /workspace, one path for every
# workspace (the worst case); with DISTINCT=1, /workspaces/ws-<w>, like the
# devcontainer default /workspaces/<repo name> that Drydock's containers get.
wsdir() { if [ -n "${DISTINCT:-}" ]; then printf '/workspaces/ws-%s' "$1"; else printf '/workspace'; fi; }
ctr_up() {
  local w="$1" cfg="${2:-$CFG}"; shift; [ $# -gt 0 ] && shift
  docker rm -f "$(ctr "$w")" >/dev/null 2>&1
  grep -qx "$(ctr "$w")" "$CTRS" 2>/dev/null || printf '%s\n' "$(ctr "$w")" >> "$CTRS"
  mkdir -p "$S/ws-$w"
  docker run -d --name "$(ctr "$w")" --label dd-spike-08=1 --hostname "ws-$w" \
    -v "$cfg:$IN_CFG" -v "$S/ws-$w:$(wsdir "$w")" -v "$S/etc:/usr/local/drydock/etc:ro" \
    -v "$CLAUDE:/usr/local/bin/claude:ro" \
    -e HOME=/home/vscode -e CLAUDE_CONFIG_DIR="$IN_CFG" -e DISABLE_AUTOUPDATER=1 \
    -e CLAUDE_ENV_FILE=/usr/local/drydock/etc/claude-env.sh \
    "$@" dd-spike-08 \
    /bin/sh -c 'echo Container started; trap "exit 0" 15; while sleep 1 & wait $!; do :; done' >/dev/null
  sleep 1
  docker inspect -f '{{.State.Running}}' "$(ctr "$w")" 2>/dev/null | grep -q true \
    || { say "container $(ctr "$w") is not running:"; docker logs "$(ctr "$w")" 2>&1 | tail -5; return 1; }
}
# x <w> <cmd...>: a docker exec as vscode in /workspace, stdin closed, bounded.
x() { local w="$1"; shift; timeout "${XT:-30}" docker exec -u vscode -w "$(wsdir "$w")" "$(ctr "$w")" "$@" 2>&1; }
# xe <w> <docker-exec-args...> -- <cmd...>: the same with extra exec args (-e).
xe() {
  local w="$1" a=(); shift
  while [ "$1" != "--" ]; do a+=("$1"); shift; done; shift
  timeout "${XT:-30}" docker exec -u vscode -w "$(wsdir "$w")" "${a[@]}" "$(ctr "$w")" "$@" 2>&1
}
C() { local w="$1"; shift; x "$w" claude "$@"; }

# Strip CSI sequences but keep OSC 8 payloads.
clean() { sed 's/\x1b\[[0-9;?]*[A-Za-z]//g' | tr -d '\r'; }
# bg <w> <args...>: `claude --bg <args>`; prints the id and records it.
bg() {
  local w="$1" out id; shift
  out=$(C "$w" --bg "$@"); echo "$?" > "$RUN/bg.rc"; out=$(printf '%s\n' "$out" | clean)
  printf '%s\n' "$out" | sed 's/^/    | /' >&2
  id=$(printf '%s\n' "$out" | grep -aoE 'backgrounded · [0-9a-f]+' | awk '{print $3}')
  [ -n "$id" ] && printf '%s %s\n' "$w" "$id" >> "$BGIDS"
  printf '%s' "$id"
}
agents() { C "$1" agents --json --all | jq -c '.[] | {id, status, state, pid, name, sessionId, cwd}' 2>/dev/null || C "$1" agents --json --all | head -5; }
# Every process in a container: pid, ppid, args.
ps_in() { docker exec "$(ctr "$1")" ps -eo pid=,ppid=,stat=,args= 2>/dev/null | grep -v ' ps -eo' | cut -c1-150; }
# The env of a pid inside a container, filtered by an ERE on NAME=. Read as
# vscode: the container's root has no CAP_SYS_PTRACE, so it may not read the
# environ of a uid-1000 process.
env_of() { [ -n "$2" ] || { echo "(no pid)"; return; }; docker exec -u vscode "$(ctr "$1")" cat "/proc/$2/environ" 2>&1 | tr '\0' '\n' | grep -aE "$3" | sort; }
# The worker pid of job <id> as `claude agents` reports it.
wpid() { C "$1" agents --json --all | jq -r --arg i "$2" '.[] | select(.id == $i) | .pid // empty' 2>/dev/null; }
# The daemon's pid in container <w>: the `daemon run` process.
dpid() { ps_in "$1" | awk '/claude.* daemon run/ {print $1; exit}'; }
# Session ids from OSC 8 targets and from plain text, separately.
osc8ids() { grep -aoE $'\e\\]8;;https://claude\\.ai/code/session_[A-Za-z0-9]+' | grep -aoE 'session_[A-Za-z0-9]+' | sort -u | tr '\n' ' '; }
plainids() { clean | grep -aoE 'claude\.ai/code/session_[A-Za-z0-9]+' | grep -aoE 'session_[A-Za-z0-9]+' | sort -u | tr '\n' ' '; }
# The state files under the shared config: daemon/, jobs/, the root daemon.* files, sessions/.
state() {
  say "    -- state in the shared config:"
  ( cd "$CFG" && for f in daemon.lock daemon.status.json; do
      [ -f "$f" ] && say "    $f: $(jq -c . "$f" 2>/dev/null | cut -c1-220)"; done
    [ -f daemon/roster.json ] && say "    daemon/roster.json: $(jq -c . daemon/roster.json | cut -c1-400)"
    say "    jobs/: $(ls jobs 2>/dev/null | tr '\n' ' ')"
    say "    sessions/: $(ls sessions 2>/dev/null | tr '\n' ' ')" )
}

# pane <name> <log> <cmd...>: a detached 200x50 pane on the private tmux server.
pane() {
  local name="$1" log="$2"; shift 2
  : > "$log"
  t kill-session -t "$name" 2>/dev/null
  t new-session -d -s "$name" -x 200 -y 50 "$(printf '%q ' "$@")"
  t pipe-pane -o -t "$name" "cat >> $(printf '%q' "$log")"
}
screen() { t capture-pane -p -t "$1" 2>/dev/null; }
typeline() { t send-keys -t "$1" -l "$2"; sleep 1; t send-keys -t "$1" Enter; sleep "${3:-3}"; }

# reap: this harness's own tmux server; then `claude stop` each recorded
# background id in its own container (the graceful stop, which archives the
# session); then docker stop and remove each recorded container (SIGTERM to
# PID 1, whose exit has the kernel SIGKILL every process left in it; SIGKILL
# after 10 s if PID 1 has not exited). Nothing not recorded is touched.
reap() {
  local w id c
  t kill-server 2>/dev/null
  if [ -f "$BGIDS" ]; then
    while read -r w id; do
      [ -n "$id" ] || continue
      docker inspect -f '{{.State.Running}}' "$(ctr "$w")" 2>/dev/null | grep -q true && C "$w" stop "$id" >/dev/null 2>&1
    done < "$BGIDS"
    rm -f "$BGIDS"
  fi
  if [ -f "$CTRS" ]; then
    while read -r c; do
      [ -n "$c" ] || continue
      docker stop -t 10 "$c" >/dev/null 2>&1; docker rm "$c" >/dev/null 2>&1
    done < "$CTRS"
    rm -f "$CTRS"
  fi
  return 0
}

# fresh_state: forget earlier modes' background state in the scratch config
# (daemon/, jobs/, sessions/, daemon.*), so a mode's listings are its own.
# Only ever the scratch config.
fresh_state() {
  case "$CFG" in "$S"/*) ;; *) say "fresh_state: $CFG is not under \$S; refusing"; exit 2 ;; esac
  rm -rf "$CFG/daemon" "$CFG/jobs" "$CFG/sessions" "$CFG/daemon.lock" "$CFG/daemon.log" "$CFG/daemon.status.json"
}
# whoruns: which container runs each registered session. A sessions/<pid>.json
# row carries `pidDomain` (linux::pid:[<inode>], the pid namespace), and pids
# overlap between containers, so the row is matched to the container whose
# pid namespace that is, and checked alive there.
whoruns() {
  local f pid dom w ns where st
  say "    -- who runs each registered session (sessions/<pid>.json pidDomain -> container):"
  for f in "$CFG"/sessions/*.json; do
    [ -f "$f" ] || continue
    pid=$(jq -r .pid "$f"); dom=$(jq -r '.pidDomain // ""' "$f"); where="no container"
    for w in a b; do
      ns=$(docker exec "$(ctr "$w")" readlink /proc/1/ns/pid 2>/dev/null) || continue
      [ "linux::$ns" = "$dom" ] || continue
      # alive only if the pid's start time (stat field 22) is the row's procStart:
      # a pid namespace's inode, and so pidDomain, can be reused by a new container.
      st=$(docker exec "$(ctr "$w")" cat "/proc/$pid/stat" 2>/dev/null | awk '{print $22}')
      if [ -z "$st" ]; then where="$w (DEAD)"
      elif [ "$st" = "$(jq -r '.procStart // ""' "$f")" ]; then where="$w (alive)"
      else where="$w (DEAD; pid $pid is now another process)"; fi
    done
    say "    pid $pid in $where: $(jq -c '{sessionId: .sessionId[0:8], jobId, bridgeSessionId}' "$f")"
  done
}
# only <ids...>: filter `agents` output to these job ids.
only() { local re; re=$(printf '%s|' "$@"); grep -aE "\"id\":\"(${re%|})\""; }

# bsid <w> <id>: wait up to 30 s for job <id>'s worker to name its Remote
# Control session in $CFG/sessions/<pid>.json; print it ("none" if it never does).
bsid() {
  local i w b=
  for i in $(seq 1 30); do
    w=$(wpid "$1" "$2")
    [ -n "$w" ] && b=$(jq -r '.bridgeSessionId // empty' "$CFG/sessions/$w.json" 2>/dev/null)
    [ -n "$b" ] && { printf '%s' "$b"; return 0; }
    sleep 1
  done
  printf 'none'
}
# The daemon log since <iso-time> (its lines are [<iso>Z] ...).
dlog_since() { awk -v t="$1" 'substr($1,2,19) >= t' "$CFG/daemon.log" 2>/dev/null | cut -c1-200 | sed 's/^/    daemon.log: /'; }
# Lines of a worker's debug file about ending its Remote Control session.
ENDRE='archiv|deregist|teardown|shutdown|SIGTERM|SIGHUP|signal|bridge.*(clos|stop|end|disconnect)|graceful|exit'
