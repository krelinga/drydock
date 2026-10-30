#!/usr/bin/env bash
# Spike 08 phase 2 — is `claude --bg --remote-control` workable as Drydock's
# per-workspace model? See ../08-bg-rc.md for the question and the results.
#
# Usage: S=<scratch> ./run.sh <mode>     (build <scratch> with ./setup.sh first)
#
#   cli        what --bg, agents, attach, logs, stop, respawn offer (help text)
#   start      one container: a headless start over docker exec, what says it is
#              up, where an id and a link come from, and whether it outlives the exec
#   gates      trust, remoteDialogSeen, hasCompletedOnboarding, oauthAccount
#              removed one at a time, each in a fresh container on its own config
#   env        which environment the daemon's session gets: the launching exec's
#              or the daemon's; CLAUDE_ENV_FILE run by a Bash tool call (one turn)
#   stop       claude stop, SIGTERM of the worker, SIGTERM of the daemon, and
#              what a restart gets back (session id, any refusal)
#   two        two containers on one shared config: roster, lock, what each lists,
#              whether either adopts, stops or reaps the other's job. Both
#              mount their clone at /workspace (the worst case); DISTINCT=1
#              mounts them at /workspaces/ws-a and /workspaces/ws-b instead
#   lifecycle  docker stop/start and rm/run of a container with a live job:
#              what is left in daemon/ and jobs/, and what the next start does
#   daemon     `claude daemon run` held in the foreground by a docker exec;
#              `claude daemon stop [--any]` and SIGTERM of that daemon
#   app        the human steps H1, H2: pauses (WAIT_APP s) for the owner at the app
#   refresh    reading only: the refresh lock path the binary uses (no refresh)
#
# Every mode reaps what it started (see lib.sh). Only `env` sends turns: one
# per job, two in all, each one Bash call (NO_TURN=1 skips them).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODE="${1:?usage: S=<scratch> ./run.sh <mode>}"
case "$MODE" in
  cli|start|gates|env|stop|two|lifecycle|daemon|app|refresh) ;;
  *) printf 'unknown mode: %s\n' "$MODE"; exit 2 ;;
esac
: "${S:?set S to the scratch dir setup.sh built}"
# DISTINCT=1 moves the clones for `two` only; every other mode reads and
# writes /workspace by name, and would silently report MISSING under it.
if [ -n "${DISTINCT:-}" ] && [ "$MODE" != two ]; then printf 'DISTINCT=1 is for the two mode only\n'; exit 2; fi
# First, before the guard can refuse: whatever a killed run left (sweep.sh).
"$HERE/sweep.sh" || exit 4
RUN="$S/run/$MODE"
. "$HERE/lib.sh"   # refuses a CFG or S that is the real config or $HOME
rm -rf "$RUN"; mkdir -p "$RUN"
# The minutes each mode may run with a daemon live, for the token guard: app
# pauses three times WAIT_APP, the rest finish in a few minutes.
W_APP="${WAIT_APP:-300}"
case "$W_APP" in ''|*[!0-9]*) say "WAIT_APP must be a whole number of seconds"; exit 2 ;; esac
case "$MODE" in app) MODE_MIN=$(( (3 * W_APP + 59) / 60 + 10 )) ;; *) MODE_MIN=10 ;; esac
token_guard "$CFG/.credentials.json" "$MODE_MIN"
trap reap EXIT
reap
fresh_state
say "claude under test: $("$CLAUDE" --version 2>&1 | head -1) ($CLAUDE)"

case "$MODE" in
# ---------------------------------------------------------------------------
cli)
  ctr_up a || exit 1
  say "--- claude --help, background lines"
  C a --help | grep -aE -- '--bg|--remote-control|--rc|^  (agents|attach|logs|stop|respawn|rm|daemon)' | sed 's/^/    /'
  for c in agents attach logs stop respawn rm daemon; do
    say "--- claude $c --help"; C a "$c" --help | sed 's/^/    /' | head -25
  done
  say "--- claude --bg --help line, in full"
  C a --help | grep -aA3 -- '--bg, --background' | sed 's/^/    /'
  ;;
# ---------------------------------------------------------------------------
start)
  ctr_up a || exit 1
  touch "$RUN/t0"
  say "--- claude --bg --remote-control ddspike08a, over docker exec (no TTY, stdin closed)"
  s0=$(date +%s%N); ID=$(bg a --remote-control ddspike08a); s1=$(date +%s%N)
  say "  id: ${ID:-NONE}; the exec returned after $(( (s1 - s0) / 1000000 )) ms"
  [ -n "$ID" ] || exit 1
  say "  processes right after the exec returned:"; ps_in a | sed 's/^/    /'
  sleep 10
  say "--- 10 s later"
  say "  claude agents --json --all:"; agents a | sed 's/^/    /'
  C a agents --json --all > "$RUN/agents.json"
  say "  claude daemon status:"; C a daemon status | sed 's/^/    /'
  state
  say "  files under jobs/$ID/:"; (cd "$CFG/jobs/$ID" 2>/dev/null && ls -la | sed 's/^/    /')
  for f in "$CFG/jobs/$ID"/*.json; do [ -f "$f" ] && say "    $(basename "$f"): $(jq -c . "$f" | cut -c1-600)"; done
  W=$(wpid a "$ID"); say "  worker pid (agents): ${W:-none}"
  [ -n "$W" ] && [ -f "$CFG/sessions/$W.json" ] && say "  sessions/$W.json: $(jq -c '{pid,sessionId,bridgeSessionId,entrypoint,kind,name,cwd}' "$CFG/sessions/$W.json")"
  C a logs "$ID" > "$RUN/logs1.txt"
  say "  claude logs: $(wc -c < "$RUN/logs1.txt") bytes; OSC 8 session ids: [$(osc8ids < "$RUN/logs1.txt")]; plain-text ids: [$(plainids < "$RUN/logs1.txt")]; env ids: [$(grep -aoE 'env_[A-Za-z0-9]+' "$RUN/logs1.txt" | sort -u | tr '\n' ' ')]"
  clean < "$RUN/logs1.txt" | grep -aiE 'remote|session_|/rc|active|environment' | cut -c1-160 | head -6 | sed 's/^/    | /'
  say "  any other file naming a session_ id under the config:"
  grep -rlaE 'session_[A-Za-z0-9]{10,}' "$CFG" --include='*.json' 2>/dev/null | sed "s|$CFG/||" | head -8 | sed 's/^/    /'
  say "  files the start wrote or changed under the config:"
  (cd "$CFG" && find . -newer "$RUN/t0" -type f | grep -v '^./projects/.*\.jsonl$' | sort | sed 's/^/    /' | head -40)
  say "--- 60 s later: does it outlive the exec?"
  sleep 60
  agents a | sed 's/^/    /'; ps_in a | grep -a claude | sed 's/^/    /'
  say "--- claude stop $ID"
  C a stop "$ID" | clean | sed 's/^/    | /'
  D=$(dpid a)
  for i in $(seq 1 30); do [ -n "$(dpid a)" ] || break; sleep 1; done
  say "  daemon (was pid $D) gone after ${i} s: $([ -z "$(dpid a)" ] && echo yes || echo no)"
  agents a | sed 's/^/    /'; state
  tail -15 "$CFG/daemon.log" | cut -c1-200 | sed 's/^/    daemon.log: /'
  say "--- result 10's scheduling line (whole log)"
  grep -a 'scheduling proactive refresh' "$CFG/daemon.log" | cut -c1-200 | sed 's/^/    scheduling: /' || say "    (no scheduling line)"
  ;;
# ---------------------------------------------------------------------------
gates)
  # Each gate in a fresh container on its own copy of the config, so one
  # gate's daemon never sees another's. `none` is the positive control.
  declare -A DEL=([none]='.' [trust]='del(.projects)' [remoteDialogSeen]='del(.remoteDialogSeen)'
                  [onboarding]='del(.hasCompletedOnboarding) | del(.theme)' [oauthAccount]='del(.oauthAccount)')
  for g in none trust remoteDialogSeen onboarding oauthAccount; do
    say "=== gate removed: $g"
    G="$S/cfg-gate-$g"; rm -rf "$G"; mkdir -p "$G"
    install -m 600 "$CFG/.credentials.json" "$G/.credentials.json"
    jq "${DEL[$g]}" "$CFG/.claude.json" > "$G/.claude.json"; chmod 600 "$G/.claude.json"
    rm -rf "$S/ws-g$g"; cp -a "$S/ws-a" "$S/ws-g$g"
    ctr_up "g$g" "$G" || continue
    ID=$(CFG="$G" bg "g$g" --remote-control "ddspike08$g")
    [ -n "$ID" ] || { say "  no id; claude --bg exited $(cat "$RUN/bg.rc")"; continue; }
    sleep 25
    say "  agents: $(agents "g$g")"
    say "  state.json: $(jq -c '{state, tempo, detail, needs, bridgeSessionId}' "$G/jobs/$ID/state.json" 2>/dev/null)"
    W=$(wpid "g$g" "$ID")
    say "  worker ${W:-none} alive: $( [ -n "$W" ] && docker exec "$(ctr "g$g")" kill -0 "$W" 2>/dev/null && echo yes || echo no); bridge session in sessions/: $(jq -r '.bridgeSessionId // "none"' "$G/sessions/$W.json" 2>/dev/null || echo 'no row')"
    C "g$g" logs "$ID" > "$RUN/logs-$g.txt"
    say "  logs, lines naming a gate:"
    clean < "$RUN/logs-$g.txt" | grep -aiE 'trust|y/n|remote control|organization|theme|dark mode|/rc|safety|Yes, |No, ' | cut -c1-180 | head -6 | sed 's/^/    | /'
    say "  .claude.json after: $(jq -c '{remoteDialogSeen, oauth: (.oauthAccount != null), hasCompletedOnboarding, trusted: (.projects // {} | keys)}' "$G/.claude.json")"
    C "g$g" stop "$ID" >/dev/null
    sleep 2
  done
  ;;
# ---------------------------------------------------------------------------
env)
  ctr_up a || exit 1
  ENVRE='^(DD_[A-Z0-9_]*|DISABLE_[A-Z_]*|DO_NOT_TRACK|CLAUDE_ENV_FILE|CLAUDE_CONFIG_DIR|ANTHROPIC_BASE_URL|CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC|CLAUDECODE|CLAUDE_CODE_[A-Z_]*)='
  say "--- exec 1 starts the daemon: DD_MARK=launch1 DD_ONLY1=x"
  ID1=$(xe a -e DD_MARK=launch1 -e DD_ONLY1=x -- claude --bg --remote-control env1 | clean | tee /dev/stderr | grep -aoE 'backgrounded · [0-9a-f]+' | awk '{print $3}')
  [ -n "$ID1" ] && echo "a $ID1" >> "$BGIDS"
  sleep 8
  say "--- exec 2, daemon already up: DD_MARK=launch2 DO_NOT_TRACK=1 DISABLE_TELEMETRY=1, and DISABLE_AUTOUPDATER and CLAUDE_ENV_FILE unset (env -u)"
  ID2=$(xe a -e DD_MARK=launch2 -e DO_NOT_TRACK=1 -e DISABLE_TELEMETRY=1 -- env -u DISABLE_AUTOUPDATER -u CLAUDE_ENV_FILE claude --bg --remote-control env2 | clean | tee /dev/stderr | grep -aoE 'backgrounded · [0-9a-f]+' | awk '{print $3}')
  [ -n "$ID2" ] && echo "a $ID2" >> "$BGIDS"
  sleep 12
  D=$(dpid a); W1=$(wpid a "$ID1"); W2=$(wpid a "$ID2")
  say "  daemon $D:";  env_of a "$D" "$ENVRE" | sed 's/^/    /'
  say "  job 1 ($ID1) worker $W1:"; env_of a "$W1" "$ENVRE" | sed 's/^/    /'
  say "  job 2 ($ID2) worker $W2:"; env_of a "$W2" "$ENVRE" | sed 's/^/    /'
  for i in "$ID1" "$ID2"; do
    say "  $i state.json: $(jq -c '{providerEnv, respawnFlags, bridgeSessionId}' "$CFG/jobs/$i/state.json")"
  done
  say "--- exec 3: claude respawn $ID2 with DD_MARK=launch3"
  xe a -e DD_MARK=launch3 -- claude respawn "$ID2" | clean | sed 's/^/    | /'
  sleep 12; W2b=$(wpid a "$ID2")
  say "  job 2 worker after respawn ${W2b}:"; env_of a "$W2b" "$ENVRE" | sed 's/^/    /'
  [ -n "${NO_TURN:-}" ] && { C a stop "$ID1" >/dev/null; C a stop "$ID2" >/dev/null; say "=== done ($MODE, NO_TURN)"; exit 0; }
  say "--- one Bash tool call in each job (attach in tmux, one turn): does CLAUDE_ENV_FILE run, and with which DD_MARK?"
  rm -f "$S/ws-a/.env-file-ran"
  for i in "$ID1" "$ID2"; do
    pane "at$i" "$RUN/attach-$i.log" docker exec -it -u vscode -w /workspace "$(ctr a)" claude attach "$i"
    sleep 8
    typeline "at$i" 'Use the Bash tool to run exactly: env | grep -E "^(DD_|DISABLE_|DO_NOT_TRACK|CLAUDE_ENV_FILE|CLAUDE_CONFIG_DIR|ANTHROPIC_BASE_URL|CLAUDECODE)" | sort > /workspace/.bash-'"$i"'  Then reply with exactly OK.' 30
    say "  $i screen tail:"; screen "at$i" | grep -v '^\s*$' | tail -6 | cut -c1-160 | sed 's/^/    | /'
    say "  /workspace/.bash-$i (the session's Bash environment):"; sed 's/^/    /' "$S/ws-a/.bash-$i" 2>/dev/null || say "    MISSING"
    t kill-session -t "at$i" 2>/dev/null
  done
  say "  /workspace/.env-file-ran:"; sed 's/^/    /' "$S/ws-a/.env-file-ran" 2>/dev/null || say "    MISSING (CLAUDE_ENV_FILE never ran)"
  C a stop "$ID1" >/dev/null; C a stop "$ID2" >/dev/null
  ;;
# ---------------------------------------------------------------------------
stop)
  ctr_up a || exit 1
  DBG=/workspace/.w.debug
  say "--- start with --debug-file (is it accepted and passed to the worker?)"
  ID=$(bg a --remote-control stopa --debug-file "$DBG")
  [ -n "$ID" ] || exit 1
  B0=$(bsid a "$ID"); W=$(wpid a "$ID")
  say "  worker $W argv: $(ps_in a | awk -v p="$W" '$1==p' | cut -c1-150)"
  say "  session: $B0; debug file: $(wc -l < "$S/ws-a/.w.debug" 2>/dev/null || echo none) lines"

  say "=== 1. claude stop"
  T=$(now); C a stop "$ID" | clean | sed 's/^/    | /'; sleep 3
  say "  agents: $(agents a)"; say "  worker $W alive: $(docker exec "$(ctr a)" kill -0 "$W" 2>/dev/null && echo yes || echo no)"
  dlog_since "$T"
  say "  worker debug lines about ending:"; grep -aiE "$ENDRE" "$S/ws-a/.w.debug" 2>/dev/null | tail -6 | cut -c1-180 | sed 's/^/    > /'
  say "--- claude respawn $ID (headless restart)"
  T=$(now); C a respawn "$ID" | clean | sed 's/^/    | /'
  B1=$(bsid a "$ID"); W=$(wpid a "$ID")
  say "  after respawn: worker $W, session $B1 (was $B0); agents: $(agents a)"
  dlog_since "$T"

  say "=== 2. SIGTERM the worker ($W)"
  : > "$S/ws-a/.w.debug"
  T=$(now); docker exec -u vscode "$(ctr a)" kill -TERM "$W"; sleep 5
  say "  agents: $(agents a)"; dlog_since "$T"
  say "  worker debug lines about ending:"; grep -aiE "$ENDRE" "$S/ws-a/.w.debug" 2>/dev/null | tail -8 | cut -c1-180 | sed 's/^/    > /'
  sleep 10; say "  15 s on, restarted unasked? agents: $(agents a)"
  say "  its session now: $(bsid a "$ID") (was $B1); the debug log since the SIGTERM:"
  grep -aE 'Created session|Reattaching|Unarchive' "$S/ws-a/.w.debug" | tail -3 | cut -c1-160 | sed 's/^/    > /'
  C a respawn "$ID" | clean | sed 's/^/    | /'
  B2=$(bsid a "$ID"); W=$(wpid a "$ID"); say "  respawn: worker $W, session $B2 (was $B1)"

  say "=== 3. SIGKILL the worker ($W), respawn at once"
  T=$(now); docker exec -u vscode "$(ctr a)" kill -KILL "$W"; sleep 2
  say "  agents: $(agents a)"; dlog_since "$T"
  C a respawn "$ID" | clean | sed 's/^/    | /'
  B3=$(bsid a "$ID"); W=$(wpid a "$ID"); say "  respawn: worker $W, session $B3 (was $B2)"
  sleep 20
  say "  20 s on: agents: $(agents a); state.json: $(jq -c '{state, detail, bridgeSessionId}' "$CFG/jobs/$ID/state.json")"
  say "  worker debug lines naming a refusal:"; grep -aiE 'already|409|refus|conflict|fail' "$S/ws-a/.w.debug" | tail -4 | cut -c1-180 | sed 's/^/    > /'

  say "=== 4. SIGTERM the daemon"
  D=$(dpid a); say "  daemon $D; processes:"; ps_in a | grep -a claude | sed 's/^/    /'
  T=$(now); docker exec -u vscode "$(ctr a)" kill -TERM "$D"; sleep 8
  say "  after: processes:"; ps_in a | grep -a claude | sed 's/^/    /'
  dlog_since "$T"; state
  say "  agents (a new client, no daemon): $(agents a)"
  say "--- a second job after that: does a new daemon adopt the first?"
  ID2=$(bg a --remote-control stopa2); sleep 10
  say "  agents: $(agents a | tr '\n' ' ')"
  say "  session of $ID: $(bsid a "$ID")"
  state
  C a stop "$ID2" >/dev/null; C a stop "$ID" >/dev/null
  ;;
# ---------------------------------------------------------------------------
two)
  ctr_up a || exit 1; ctr_up b || exit 1
  say "--- A starts a job (A's daemon starts)"
  JA=$(bg a --remote-control twoa); [ -n "$JA" ] || exit 1
  BA=$(bsid a "$JA"); say "  A's job $JA, session $BA"
  state
  say "  B, before it runs anything: claude daemon status"; C b daemon status | sed 's/^/    b| /' | head -14
  say "  B: claude agents --json --all"; agents b | sed 's/^/    b| /'
  say "  B: claude logs $JA"; C b logs "$JA" | clean | tr -s ' ' | cut -c1-160 | head -3 | sed 's/^/    b| /'
  say "--- B starts a job (does B start a daemon of its own, and what does it do with A's job?)"
  T=$(now)
  JB=$(bg b --remote-control twob); [ -n "$JB" ] || exit 1
  BB=$(bsid b "$JB"); say "  B's job $JB, session $BB"
  sleep 5
  say "  daemons: A $(dpid a), B $(dpid b)"
  dlog_since "$T"
  state
  say "  A lists:"; agents a | sed 's/^/    a| /'
  say "  B lists:"; agents b | sed 's/^/    b| /'
  say "  A's job still served in A? worker $(wpid a "$JA") session $(bsid a "$JA") (was $BA); processes in A:"
  ps_in a | grep -a 'claude' | sed 's/^/    a| /'
  say "  processes in B:"; ps_in b | grep -a 'claude' | sed 's/^/    b| /'
  whoruns
  say "--- 30 s on: does either daemon change the other's job?"
  sleep 30
  dlog_since "$T" | tail -6
  say "  A lists:"; agents a | sed 's/^/    a| /'
  say "  B lists:"; agents b | sed 's/^/    b| /'
  state
  say "--- B: claude stop $JA (A's job, from B)"
  T=$(now); C b stop "$JA" | clean | sed 's/^/    b| /'; sleep 5
  say "  A's job in A: worker $(wpid a "$JA"); A lists: $(agents a | only "$JA")"
  ps_in a | grep -a 'claude --session-id' | sed 's/^/    a| /'
  dlog_since "$T"
  say "--- B: claude respawn $JA (A's job, from B): where does it run?"
  T=$(now); C b respawn "$JA" | clean | sed 's/^/    b| /'; sleep 10
  say "  processes in A:"; ps_in a | grep -a 'claude --session-id' | sed 's/^/    a| /'
  say "  processes in B:"; ps_in b | grep -a 'claude --session-id' | sed 's/^/    b| /'
  say "  A lists: $(agents a | only "$JA")"; say "  B lists: $(agents b | only "$JA")"
  whoruns
  dlog_since "$T"
  say "--- A stops its job from A, B its own; do both daemons exit?"
  C a stop "$JA" >/dev/null; C b stop "$JA" >/dev/null; C b stop "$JB" >/dev/null
  for i in $(seq 1 20); do [ -z "$(dpid a)$(dpid b)" ] && break; sleep 1; done
  say "  after ${i} s: daemon A '$(dpid a)', daemon B '$(dpid b)'"
  state
  ;;
# ---------------------------------------------------------------------------
lifecycle)
  ctr_up a || exit 1
  ID=$(bg a --remote-control lifea --debug-file /workspace/.w.debug); [ -n "$ID" ] || exit 1
  B0=$(bsid a "$ID"); say "  job $ID, session $B0"
  for round in stop-start rm-run; do
    say "=== $round: the container goes away with the job live"
    : > "$S/ws-a/.w.debug"
    T=$(now); s0=$(date +%s)
    if [ "$round" = stop-start ]; then docker stop -t 10 "$(ctr a)" >/dev/null
    else docker rm -f "$(ctr a)" >/dev/null; fi
    say "  gone after $(( $(date +%s) - s0 )) s; worker debug lines about ending (none = no teardown ran):"
    grep -aiE "$ENDRE" "$S/ws-a/.w.debug" | tail -4 | cut -c1-160 | sed 's/^/    > /'
    dlog_since "$T"; state
    say "  $ID state.json: $(jq -c '{state, detail, bridgeSessionId}' "$CFG/jobs/$ID/state.json")"
    if [ "$round" = stop-start ]; then docker start "$(ctr a)" >/dev/null; else ctr_up a || exit 1; fi
    sleep 3
    say "--- back up: is a daemon running before anything asks? '$(dpid a)'"
    say "  claude daemon status:"; C a daemon status | grep -aE 'not running|warning|bg workers|pid:' | sed 's/^/    | /'
    T=$(now)
    say "  claude agents --json --all:"; agents a | sed 's/^/    /'
    sleep 3; say "  daemon after that listing: '$(dpid a)'"; dlog_since "$T"
    say "--- a new job beside the stale entry"
    T=$(now); N=$(bg a --remote-control "life$round"); say "  new job ${N:-NONE}, session $( [ -n "$N" ] && bsid a "$N")"
    dlog_since "$T"
    say "  agents: $(agents a | tr '\n' ' ')"
    say "--- claude respawn $ID (the job the container took down)"
    T=$(now); C a respawn "$ID" | clean | sed 's/^/    | /'
    say "  session after respawn: $(bsid a "$ID") (was $B0)"
    grep -aE 'Reattaching|Unarchive|Created session' "$S/ws-a/.w.debug" | tail -3 | cut -c1-160 | sed 's/^/    > /'
    dlog_since "$T"; whoruns
    [ -n "$N" ] && C a stop "$N" >/dev/null
  done
  C a stop "$ID" >/dev/null
  ;;
# ---------------------------------------------------------------------------
daemon)
  ctr_up a || exit 1
  say "--- claude daemon run, held in the foreground by a docker exec (as a supervisor would hold it)"
  pane fgd "$RUN/fgd.log" docker exec -u vscode -w /workspace "$(ctr a)" claude daemon run
  sleep 4
  D=$(dpid a); say "  daemon pid ${D:-none}: $(ps_in a | awk -v p="$D" '$1==p' | cut -c1-120)"
  say "  claude daemon status:"; C a daemon status | grep -aE 'pid:|origin:|control.sock' | sed 's/^/    | /'
  ID=$(bg a --remote-control fga --debug-file /workspace/.w.debug); [ -n "$ID" ] || exit 1
  B0=$(bsid a "$ID")
  say "  job $ID, session $B0; its pty host's parent: $(ps_in a | awk -v w="$(wpid a "$ID")" '$1==w {print $2}' | xargs -I{} docker exec "$(ctr a)" ps -o ppid= -p {} | tr -d ' ') (daemon $D)"
  say "--- the job stopped: does a foreground daemon idle-exit?"
  C a stop "$ID" >/dev/null; sleep 10
  say "  daemon after 10 s: '$(dpid a)'"; dlog_since "$(date -u -d '-15 sec' +%Y-%m-%dT%H:%M:%S)" | tail -3
  C a respawn "$ID" >/dev/null; B1=$(bsid a "$ID"); say "  respawned: session $B1 (was $B0)"
  say "--- claude daemon stop (no --any), with the job live"
  C a daemon stop | clean | sed 's/^/    | /'
  say "--- claude daemon stop --any, with the job live"
  : > "$S/ws-a/.w.debug"; T=$(now)
  C a daemon stop --any | clean | sed 's/^/    | /'; sleep 6
  say "  daemon: '$(dpid a)'; the holding pane: $(t list-panes -t fgd -F '#{pane_dead}' 2>/dev/null || echo gone); worker: '$(wpid a "$ID")'"
  ps_in a | grep -a claude | sed 's/^/    /'
  dlog_since "$T"
  say "  worker debug lines about ending:"; grep -aiE "$ENDRE" "$S/ws-a/.w.debug" | grep -av 'rg error|cc-plugin' | tail -4 | cut -c1-160 | sed 's/^/    > /'
  say "  agents: $(agents a)"
  say "--- again, and this time SIGTERM the foreground daemon inside the container"
  pane fgd "$RUN/fgd2.log" docker exec -u vscode -w /workspace "$(ctr a)" claude daemon run
  sleep 4; D=$(dpid a)
  C a respawn "$ID" >/dev/null; B2=$(bsid a "$ID"); say "  daemon $D; respawned: session $B2 (was $B1)"
  : > "$S/ws-a/.w.debug"; T=$(now)
  docker exec -u vscode "$(ctr a)" kill -TERM "$D"; sleep 6
  say "  daemon: '$(dpid a)'; the holding pane: $(t list-panes -t fgd -F '#{pane_dead}' 2>/dev/null || echo gone)"
  ps_in a | grep -a claude | sed 's/^/    /'
  dlog_since "$T"
  say "  worker debug lines about ending:"; grep -aiE "$ENDRE" "$S/ws-a/.w.debug" | grep -av 'rg error|cc-plugin' | tail -4 | cut -c1-160 | sed 's/^/    > /'
  say "  agents: $(agents a)"
  ;;
# ---------------------------------------------------------------------------
app)
  # The human steps H1 and H2 (README): the owner looks at the Claude app at
  # each pause. WAIT_APP (default 300) seconds per pause (W_APP, set above,
  # and counted by the token guard); WAIT_APP=0 dry-runs.
  ctr_up a || exit 1
  pane fgd "$RUN/fgd.log" docker exec -u vscode -w /workspace "$(ctr a)" claude daemon run
  sleep 4
  ID=$(bg a --remote-control ddspike08app --debug-file /workspace/.w.debug); [ -n "$ID" ] || exit 1
  B0=$(bsid a "$ID")
  say "H1: open https://claude.ai/code/$B0 (or find 'ddspike08app' in the app's Remote Control"
  say "    section). Send: Reply with exactly APP1 and nothing else.   Waiting $W_APP s."
  sleep "$W_APP"
  C a logs "$ID" | clean | grep -aoE 'APP1' | head -1 | sed 's/^/  seen in claude logs: /'
  say "  bridge turns in the worker's debug log: $(grep -acE 'remote-bridge\] (Sending|Sent)' "$S/ws-a/.w.debug")"
  say "--- claude stop $ID (the worker's debug log should say archive=200)"
  C a stop "$ID" >/dev/null; sleep 3
  grep -aE 'Archive|Torn down' "$S/ws-a/.w.debug" | tail -2 | cut -c1-140 | sed 's/^/    > /'
  say "H2a: in the app, what does the session show now (archived, offline, gone)? Waiting $W_APP s."
  sleep "$W_APP"
  C a respawn "$ID" >/dev/null; B1=$(bsid a "$ID")
  say "--- claude respawn $ID: session $B1 (was $B0)"
  grep -aE 'Reattaching|Unarchive|Created session' "$S/ws-a/.w.debug" | tail -2 | cut -c1-140 | sed 's/^/    > /'
  say "H2b: is it back in the app, with APP1 in its history? Send: Reply with exactly APP2."
  say "    Waiting $W_APP s."
  sleep "$W_APP"
  C a logs "$ID" | clean | grep -aoE 'APP[12]' | sort -u | tr '\n' ' ' | sed 's/^/  seen in claude logs: /'; echo
  C a daemon stop --any | clean | sed 's/^/    | /'
  ;;
# ---------------------------------------------------------------------------
refresh)
  # Reading only: no claude is run, no refresh is attempted. Prints the
  # bundled JavaScript around each string, for a human to read.
  ctx() { # ctx <string> <before> <after> [ERE]: hits whose context matches ERE (default: source text, not the bytecode copies)
    grep -aboF "$1" "$CLAUDE" | cut -d: -f1 | while read -r off; do
      dd if="$CLAUDE" bs=1 skip=$(( off - $2 )) count=$(( $2 + $3 )) 2>/dev/null | tr -c '[:print:]' '.' > "$RUN/ctx.$off"
      grep -aqE "${4:-function|=>}" "$RUN/ctx.$off" || continue
      say "--- '$1' at byte $off"; { fold -w 160 "$RUN/ctx.$off"; echo; } | sed 's/^/    /'
    done
  }
  say "=== the refresh lock: its path, staleness and update interval, and its owner record"
  ctx '.oauth_refresh.lock' 700 500
  say "=== the daemon's proactive refresh: when it fires and what it calls (E() imports the auth chunk)"
  ctx 'auth: scheduling proactive refresh' 300 1400
  say "=== the auth chunk's exports: the CLI's refresh and the refresh-lock hold side by side"
  ctx 'as checkAndRefreshOAuthTokenIfNeeded,' 500 300 'beginOAuthRefreshLockHold'
  ;;
esac
say "=== done ($MODE)"
