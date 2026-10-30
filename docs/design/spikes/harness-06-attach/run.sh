#!/usr/bin/env bash
# Spike 06 — can an operator in a terminal get onto the Claude session the app
# sees? See ../06-terminal-attach.md for the question and the results.
#
# Usage: S=<scratch> ./run.sh <mode>     (build <scratch> with ./setup.sh first)
#
#   cli                 path 3: what 2.1.289 offers (--help, strings, --cloud, attach, agents)
#   server-resume       path 1: --resume/--continue/picker/--teleport against the
#                       server's own session, live and after SIGTERM. With
#                       WAIT_APP=<s> it first waits that long for a turn sent
#                       from the Claude app (the human step), so the session
#                       has a transcript to resume.
#   interactive-resume  path 1's mechanics, unattended: two terminals on one
#                       conversation, turns in each, then the takeover
#   alongside           path 2: interactive Remote Control beside the server,
#                       in the same folder and in a git worktree of it
#   tmux-rc             path 4: `claude --remote-control` in tmux instead of
#                       the server: ids, turns, a client attach/resize,
#                       SIGTERM and SIGKILL with restart
#   tmux-rc-app         path 4's human half: a turn sent from the app, seen in
#                       the terminal (WAIT_APP=<s>, default 300)
#   tmux-rc-gates       path 4: the config gates, one removed at a time
#   bg                  path 3/4: `claude --bg --remote-control` and `claude attach`
#
# Every mode reaps what it started (SIGTERM first; see lib.sh). Turns sent are
# one-word replies, and few: they spend the signed-in account's quota.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODE="${1:?usage: S=<scratch> ./run.sh <mode>}"
# Each mode logs to a fresh $S/run/<mode>/, so a debug file (which claude
# appends to) never carries an earlier run's lines.
: "${S:?set S to the scratch dir setup.sh built}"
RUN="$S/run/$MODE"; rm -rf "$RUN"
. "$HERE/lib.sh"

# mark <pane> <log>: the lines of a terminal this spike's verdicts rest on.
MARKERS='No conversation found|Remote Control not started here|already has Remote Control|Session resumed|own copy of the session|remote-control is active|/rc active|standing down|code 4090|already served|already in use|Trust|trust this|Enable Remote Control|organization|Error:|Remote Control (failed|disconnected)|Resume this session with|claude --resume'
mark() {
  { screen "$1"; clean "$2"; } | grep -aoE "[^│]{0,40}($MARKERS)[^│]{0,90}" | sed 's/  */ /g' | sort -u | sed 's/^/    | /' | head -12
}
turns() { # turns <transcript>: user/assistant record count
  [ -f "$1" ] || { echo 0; return; }
  jq -s 'map(select(.type == "user" or .type == "assistant")) | length' "$1" 2>/dev/null || echo "?"
}
dbg_since() { # dbg_since <debug-file> <iso-time> <ERE>
  [ -f "$1" ] || return 0
  awk -v t="$2" '$1 >= t' "$1" | grep -aE "$3" | cut -c1-200 | sed 's/^/    > /' | tail -8
}
now() { date -u +%Y-%m-%dT%H:%M:%S; }
trap reap EXIT

say "claude under test: $("$CLAUDE" --version 2>&1 | head -1)"
say "ws=$WS cfg=$CFG run=$RUN"
reap

case "$MODE" in
# ---------------------------------------------------------------------------
cli)
  say "=== path 3: what the CLI offers"
  say "--- claude --help: lines about resuming, attaching, remote control, background"
  cenv "$CLAUDE" --help 2>&1 | grep -aE -- '--resume|--continue|--fork-session|--teleport|--cloud|--remote-control|--bg|--session-id|--desktop|^  attach|^  agents|^  logs|^  stop|^  respawn' | sed 's/^/    /'
  say "--- claude remote-control --help: the session flags"
  cenv "$CLAUDE" remote-control --help 2>&1 | grep -aE -A3 -- '--continue|--session-id|--spawn' | sed 's/^/    /'
  say "--- binary strings: slash commands near remote control / attach"
  bin="$(readlink -f "$CLAUDE")"
  strings -n 6 "$bin" | grep -aoE '"/(remote-control|rc|teleport|attach|resume|bridge|share|remote-env)[^"]{0,24}"' | sort | uniq -c | sed 's/^/    /'
  say "--- live probes against a running server"
  start_server "$RUN/cli-srv.log" || exit 1
  B=$(sessids "$RUN/cli-srv.log" | awk '{print $1}'); U=$(uuid_of "$B")
  say "  server session: $B  local id: $U"
  CP=$(live_rows | jq -r --arg b "$B" 'select(.bridgeSessionId == $b and .entrypoint == "sdk-cli") | .pid' | head -1)
  say "  that session's process (pid $CP) is started with:"
  tr '\0' '\n' < "/proc/$CP/cmdline" | grep -aE '^--(print|sdk-url|session-id|resume|input-format)' | sed 's/^/    /'
  for probe in "--cloud $B" "attach ${B/session_/cse_}" "attach $U" "--session-id $U" "agents"; do
    # shellcheck disable=SC2086
    pane probe "$WS" "$RUN/cli-probe.log" "$CLAUDE" $probe
    sleep 10
    say "  claude $probe"
    { screen probe; clean "$RUN/cli-probe.log"; } | grep -aoE '(Error|No job|not enabled|already in use|[0-9]+ awaiting input[^│]*|Working)[^│]{0,110}' | sort -u | sed 's/^/    | /' | head -4
    term probe
  done
  say "  claude agents --json (\"active sessions (interactive and background)\") with the server up:"
  (cd "$WS" && cenv "$CLAUDE" agents --json) | jq -c '.[] | {id, kind, status, sessionId}' 2>/dev/null | sed 's/^/    /'
  say "  (a row with id null is listed but has no id that claude attach takes)"
  ;;
# ---------------------------------------------------------------------------
server-resume)
  say "=== path 1 against the server's own session"
  start_server "$RUN/sr-srv.log" || exit 1
  B=$(sessids "$RUN/sr-srv.log" | awk '{print $1}'); E=$(envid "$RUN/sr-srv.log")
  U=$(uuid_of "$B") || { say "no registry row names $B"; exit 1; }
  F="$(projdir "$WS")/$U.jsonl"
  SDBG="$RUN/sr-srv.log-${B/session_/cse_}.debug"
  say "  server: env=$E session=$B local id=$U  $(cap "$RUN/sr-srv.log")"
  if [ -n "${WAIT_APP:-}" ]; then
    say "  HUMAN STEP: open https://claude.ai/code?environment=$E , open the session"
    say "  named $(clean "$RUN/sr-srv.log" | grep -aoE 'ddspike06-[a-z]+-[a-z]+' | tail -1), and send: Reply with exactly APP1 and nothing else."
    say "  waiting up to ${WAIT_APP}s for that turn to reach $F"
    for _ in $(seq 1 "$WAIT_APP"); do [ "$(turns "$F")" -ge 2 ] && break; sleep 1; done
  fi
  say "  transcript: $(wc -c < "$F" 2>/dev/null || echo 0) bytes, $(turns "$F") user/assistant records"

  say "--- (a) claude --resume <local id> while the server holds it"
  T0=$(now)
  pane r1 "$WS" "$RUN/sr-r1.log" "$CLAUDE" --resume "$U" --debug-file "$RUN/sr-r1.debug"; sleep 14
  mark r1 "$RUN/sr-r1.log"
  say "  processes holding $U:"; holders "$U" | sed 's/^/    /'
  if [ "$(turns "$F")" -ge 2 ] && panepid r1 >/dev/null && kill -0 "$(panepid r1)" 2>/dev/null; then
    typeline r1 'Reply with exactly TERM1 and nothing else.' 15
    say "  after a turn typed in the resumed terminal:"; TREE "$F" | sed 's/^/    /'
    say "  server session's own debug since the resume (inbound? superseded?):"
    dbg_since "$SDBG" "$T0" 'superseded|4090|state=|inbound|Received|user message'
    say "--- (b) /remote-control in the resumed terminal: does it take the session from the server?"
    T1=$(now); typeline r1 '/remote-control ' 15
    mark r1 "$RUN/sr-r1.log"
    dbg_since "$SDBG" "$T1" 'superseded|4090|state=|ended|exit'
    say "  server pane now:"; screen srv | grep -aE 'Capacity|Session|ended|failed|disconnect' | tail -4 | sed 's/^/    | /'
    say "  processes holding $U:"; holders "$U" | sed 's/^/    /'
  fi
  term r1

  say "--- (c) claude --continue in the same folder"
  pane c1 "$WS" "$RUN/sr-c1.log" "$CLAUDE" --continue; sleep 12
  mark c1 "$RUN/sr-c1.log"
  say "  registry:"; registry | sed 's/^/    /'
  if holders "$U" | grep -q '"entrypoint":"cli"'; then
    say "  --continue opened the server's own session ($U) beside it; a turn typed there:"
    T0=$(now); typeline c1 'Reply with exactly TERM1 and nothing else.' 15
    say "    terminal's answer: $(screen c1 | grep -aE '^● ' | tail -1)"
    say "    the shared transcript $F, read as a tree:"; TREE "$F" | sed 's/^/      /'
    say "    the server session's debug since (did its process notice?):"
    dbg_since "$SDBG" "$T0" 'inbound|Inbound|Received|user message|reload|changed'
    say "    (nothing above = the server's session never saw the terminal's turn)"
    say "  /remote-control in that terminal: does it take the server's session over?"
    T1=$(now); typeline c1 '/remote-control ' 15
    say "    terminal now: $(screen c1 | grep -aoE '(remote-control is active|Remote Control (failed|not started)|already)[^│]{0,100}' | tail -1)"
    say "    its session: $(rcline "$RUN/sr-c1.log") (the server's is $B)"
    for i in $(seq 1 90); do grep -aq 'code=4090' "$SDBG" 2>/dev/null && break; sleep 1; done
    say "    the server's session process after ~$((15 + i)) s:"
    dbg_since "$SDBG" "$T1" '4090|superseded|state=failed'
    say "    server pane: $(screen srv | grep -aoE 'Capacity: [0-9]+/[0-9]+' | tail -1); session process alive: $(kill -0 "$(live_rows | jq -r --arg b "$B" 'select(.entrypoint == "sdk-cli" and .bridgeSessionId == $b) | .pid' | head -1)" 2>/dev/null && echo yes || echo no)"
    say "  registry:"; registry | sed 's/^/    /'
  fi
  term c1

  say "--- (d) the interactive /resume picker (claude --resume, no id)"
  pane p1 "$WS" "$RUN/sr-p1.log" "$CLAUDE" --resume; sleep 10
  screen p1 | grep -aE '❯|bytes|ago|No sessions' | sed 's/^/    | /' | head -10
  term p1

  say "--- (e) claude --teleport <bridge session id>"
  pane tp "$WS" "$RUN/sr-tp.log" "$CLAUDE" --teleport "$B" --debug-file "$RUN/sr-tp.debug"; sleep 15
  mark tp "$RUN/sr-tp.log"
  grep -aE '\[teleport\] (Fetched|Filtered)' "$RUN/sr-tp.debug" 2>/dev/null | cut -c26-160 | sed 's/^/    > /'
  term tp

  say "--- (f) SIGTERM the server, then claude --resume <local id>"
  term srv; sleep 2
  clean "$RUN/sr-srv.log" | grep -aoE 'Environment preserved[^.]*|Shutting down [0-9]+ active session' | sort -u | sed 's/^/    | /'
  pane r3 "$WS" "$RUN/sr-r3.log" "$CLAUDE" --resume "$U" --debug-file "$RUN/sr-r3.debug"; sleep 14
  mark r3 "$RUN/sr-r3.log"
  say "  processes holding $U:"; holders "$U" | sed 's/^/    /'
  say "--- (g) restart the server while that terminal holds the conversation"
  T2=$(now)
  start_server "$RUN/sr-srv2.log" && say "  server back: env=$(envid "$RUN/sr-srv2.log") sessions=$(sessids "$RUN/sr-srv2.log") $(cap "$RUN/sr-srv2.log")"
  sleep 5; mark r3 "$RUN/sr-r3.log"
  dbg_since "$RUN/sr-r3.debug" "$T2" '4090|superseded|state='
  say "  processes holding $U:"; holders "$U" | sed 's/^/    /'
  ;;
# ---------------------------------------------------------------------------
interactive-resume)
  say "=== path 1's mechanics on a conversation with turns (an interactive Remote Control session)"
  pane ia "$WS" "$RUN/ir-ia.log" "$CLAUDE" --remote-control ddspike06ia --debug-file "$RUN/ir-ia.debug"
  waitscreen 30 ia 'remote-control is active' || { say "ia never became active"; mark ia "$RUN/ir-ia.log"; exit 1; }
  B=$(rcline "$RUN/ir-ia.log"); U=$(uuid_of "$B"); F="$(projdir "$WS")/$U.jsonl"
  say "  ia: bridge=$B local id=$U"
  typeline ia 'Reply with exactly OK1 and nothing else.' 15
  say "--- a second terminal: claude --resume $U while ia is live"
  pane r2 "$WS" "$RUN/ir-r2.log" "$CLAUDE" --resume "$U" --debug-file "$RUN/ir-r2.debug"; sleep 14
  mark r2 "$RUN/ir-r2.log"
  say "  processes holding $U:"; holders "$U" | sed 's/^/    /'
  typeline r2 'Reply with exactly OK2 and nothing else.' 15
  typeline ia 'What did you reply last time? Answer with that word only.' 15
  say "  ia's answer: $(screen ia | grep -aE '^● ' | tail -1)"
  say "  the transcript, read as a tree:"; TREE "$F" | sed 's/^/    /'
  say "--- /remote-control in r2 (the one that was refused Remote Control)"
  T1=$(now); typeline r2 '/remote-control ' 15
  mark r2 "$RUN/ir-r2.log" | grep -aE 'active|standing'
  # The displaced side learns of it on its next poll, which took about a
  # minute in this spike's first attempt; wait up to 120 s.
  for i in $(seq 1 120); do grep -aq 'code=4090' "$RUN/ir-ia.debug" && break; sleep 1; done
  say "  ia told it was displaced after ~$((15 + i)) s:"
  mark ia "$RUN/ir-ia.log" | grep -aE 'standing|4090'
  dbg_since "$RUN/ir-ia.debug" "$T1" '4090|superseded'
  say "  same bridge id in r2? $(rcline "$RUN/ir-r2.log")"
  term r2; term ia
  say "--- both gone: claude --continue — which branch does it open?"
  pane c1 "$WS" "$RUN/ir-c1.log" "$CLAUDE" --continue; sleep 14
  screen c1 | grep -aE '^(❯|●) ' | sed 's/^/    | /' | tail -8
  say "  registry:"; registry | sed 's/^/    /'
  say "--- claude --teleport $B (a Remote Control session with turns)"
  pane tp "$WS" "$RUN/ir-tp.log" "$CLAUDE" --teleport "$B" --debug-file "$RUN/ir-tp.debug"; sleep 15
  mark tp "$RUN/ir-tp.log"
  grep -aE '\[teleport\] (Fetched|Filtered)' "$RUN/ir-tp.debug" 2>/dev/null | cut -c26-160 | sed 's/^/    > /'
  ;;
# ---------------------------------------------------------------------------
alongside)
  say "=== path 2: an interactive Remote Control beside the server"
  start_server "$RUN/al-srv.log" || exit 1
  say "  server: env=$(envid "$RUN/al-srv.log") $(cap "$RUN/al-srv.log")"
  say "--- (a) claude --remote-control, same folder"
  pane ia "$WS" "$RUN/al-ia.log" "$CLAUDE" --remote-control ddspike06ia --debug-file "$RUN/al-ia.debug"; sleep 15
  mark ia "$RUN/al-ia.log"
  say "  its session: $(rcline "$RUN/al-ia.log"); OSC 8 ids in its output: [$(sessids "$RUN/al-ia.log")]; env ids: [$(grep -aoE 'env_[A-Za-z0-9]+' "$RUN/al-ia.log" | sort -u | tr '\n' ' ')]"
  grep -aE '\[remote-bridge\] Created session|environments/bridge|Registered environment' "$RUN/al-ia.debug" | cut -c26-150 | sed 's/^/    > /'
  say "  server after: $(screen srv | grep -aoE 'Capacity: [0-9]+/[0-9]+' | tail -1)"
  say "--- (b) the slash command: plain claude, then /remote-control"
  pane ib "$WS" "$RUN/al-ib.log" "$CLAUDE"; sleep 10
  typeline ib '/remote-control ' 12
  mark ib "$RUN/al-ib.log"
  say "--- (c) a git worktree of the clone (where --spawn worktree puts sessions)"
  WT="$S/ws-wt"
  [ -d "$WT" ] || git -C "$WS" worktree add -q -b spike06-wt "$WT" 2>&1 | sed 's/^/    /'
  pane iw "$WT" "$RUN/al-iw.log" "$CLAUDE" --remote-control ddspike06wt --debug-file "$RUN/al-iw.debug"; sleep 15
  mark iw "$RUN/al-iw.log"
  say "  trusted in config? $(jq -r --arg p "$WT" '.projects[$p].hasTrustDialogAccepted // "no record"' "$CFG/.claude.json")"
  say "  its session: $(rcline "$RUN/al-iw.log")"
  say "  registry:"; registry | sed 's/^/    /'
  ;;
# ---------------------------------------------------------------------------
tmux-rc)
  say "=== path 4: claude --remote-control in tmux, instead of the server"
  pane rc "$WS" "$RUN/t4-rc.log" "$CLAUDE" --remote-control ddspike06rc --debug-file "$RUN/t4-rc.debug"
  waitscreen 30 rc 'remote-control is active' || { say "never became active"; mark rc "$RUN/t4-rc.log"; exit 1; }
  B=$(rcline "$RUN/t4-rc.log"); U=$(uuid_of "$B")
  say "  session=$B local id=$U"
  say "  Environment ID printed? [$(clean "$RUN/t4-rc.log" | grep -aoE 'Environment ID: env_[A-Za-z0-9]+' | head -1)]  OSC 8 session ids: [$(sessids "$RUN/t4-rc.log")]"
  say "  occurrences of the session id in its output: $(grep -aoc "$B" "$RUN/t4-rc.log")"
  grep -aE '\[remote-bridge\] Created session|environments/bridge' "$RUN/t4-rc.debug" | cut -c26-150 | sed 's/^/    > /'
  say "--- a turn typed in the terminal"
  T1=$(now); typeline rc 'Reply with exactly OK1 and nothing else.' 15
  dbg_since "$RUN/t4-rc.debug" "$T1" 'remote-bridge\] (Sending|Sent)'
  say "--- a second tmux client attaches at 80x24, then vanishes (abrupt detach)"
  T2=$(now)
  tmux -L dd-spike-06-client kill-server 2>/dev/null
  tmux -L dd-spike-06-client new-session -d -x 80 -y 24 "env -u TMUX tmux -L $TMUX_L attach -t rc"
  sleep 6
  say "  rc window while attached: $(t display -p -t rc '#{window_width}x#{window_height} clients=#{session_attached}')"
  tmux -L dd-spike-06-client kill-server 2>/dev/null; sleep 4
  t resize-window -t rc -x 200 -y 50 2>/dev/null; sleep 3
  say "  after: $(t display -p -t rc '#{window_width}x#{window_height} clients=#{session_attached} dead=#{pane_dead}')"
  dbg_since "$RUN/t4-rc.debug" "$T2" 'handleStateChange|4090|disconnect|reconnect'
  say "  (no state-change line above = the bridge did not notice)"
  say "--- SIGTERM, then claude --remote-control --continue"
  term rc; sleep 1
  clean "$RUN/t4-rc.log" | grep -aoE 'Resume this session with:|claude --resume [0-9a-f-]+' | sort -u | sed 's/^/    | /'
  pane rc "$WS" "$RUN/t4-rc2.log" "$CLAUDE" --remote-control ddspike06rc --continue --debug-file "$RUN/t4-rc2.debug"
  waitscreen 30 rc 'remote-control is active|Error|already' ; sleep 2
  mark rc "$RUN/t4-rc2.log"
  grep -aE 'Reattaching to persisted|Created session' "$RUN/t4-rc2.debug" | cut -c26-170 | sed 's/^/    > /'
  say "  after SIGTERM restart: session=$(rcline "$RUN/t4-rc2.log") (was $B); local id $(uuid_of "$(rcline "$RUN/t4-rc2.log")") (was $U)"
  say "--- SIGKILL, then the same restart at once"
  kill -KILL "$(panepid rc)"; sleep 1
  pane rc "$WS" "$RUN/t4-rc3.log" "$CLAUDE" --remote-control ddspike06rc --continue --debug-file "$RUN/t4-rc3.debug"
  waitscreen 30 rc 'remote-control is active|Error|already' ; sleep 2
  mark rc "$RUN/t4-rc3.log"
  grep -aE 'Reattaching to persisted|Created session' "$RUN/t4-rc3.debug" | cut -c26-170 | sed 's/^/    > /'
  say "  after SIGKILL restart: session=$(rcline "$RUN/t4-rc3.log") (was $B)"
  typeline rc 'What did you reply last time? Answer with that word only.' 15
  say "  conversation carried over? answer: $(screen rc | grep -aE '^● ' | tail -1)"
  say "--- a second claude --remote-control in the same folder, beside it"
  pane rc2 "$WS" "$RUN/t4-rc4.log" "$CLAUDE" --remote-control ddspike06rc2; sleep 15
  mark rc2 "$RUN/t4-rc4.log"
  say "  second session: $(rcline "$RUN/t4-rc4.log")"
  say "--- and claude remote-control (the server) in the same folder, beside it"
  pane srv "$WS" "$RUN/t4-srv.log" "$CLAUDE" "${RC_ARGS[@]}"; sleep 15
  clean "$RUN/t4-srv.log" | grep -aoE 'Environment ID: env_[A-Za-z0-9]+|Capacity: [0-9]+/[0-9]+|Error:[^·]{0,120}' | sort -u | sed 's/^/    | /'
  ;;
# ---------------------------------------------------------------------------
tmux-rc-app)
  say "=== path 4, the human half: does a turn sent from the app show in the terminal?"
  pane rc "$WS" "$RUN/t4a-rc.log" "$CLAUDE" --remote-control ddspike06app --debug-file "$RUN/t4a-rc.debug"
  waitscreen 30 rc 'remote-control is active' || { say "never became active"; exit 1; }
  B=$(rcline "$RUN/t4a-rc.log"); U=$(uuid_of "$B"); F="$(projdir "$WS")/$U.jsonl"
  say "  HUMAN STEP: open https://claude.ai/code/$B (session ddspike06app) and send:"
  say "    Reply with exactly APP1 and nothing else."
  say "  waiting up to ${WAIT_APP:-300}s for it"
  for _ in $(seq 1 "${WAIT_APP:-300}"); do screen rc | grep -q 'APP1' && break; sleep 1; done
  sleep 10
  say "  the terminal now shows:"; screen rc | grep -aE '^(❯|●) ' | tail -4 | sed 's/^/    | /'
  say "  transcript:"; TREE "$F" | sed 's/^/    /'
  ;;
# ---------------------------------------------------------------------------
tmux-rc-gates)
  say "=== path 4: the config gates, one removed at a time"
  for g in trust dialog account onboarding; do
    G="$S/cfg-$g"; rm -rf "$G"; mkdir -p "$G"
    install -m 600 "$S/cfg/.credentials.json" "$G/.credentials.json"
    case "$g" in
      trust)   f='del(.projects)' ;;
      dialog)  f='del(.remoteDialogSeen)' ;;
      account) f='del(.oauthAccount)' ;;
      onboarding) f='del(.hasCompletedOnboarding)' ;;
    esac
    jq "$f" "$S/cfg/.claude.json" > "$G/.claude.json"; chmod 600 "$G/.claude.json"
    say "--- without $g ($f)"
    CFG="$G" pane gate "$WS" "$RUN/t4-gate-$g.log" "$CLAUDE" --remote-control ddspike06gate \
      --debug-file "$RUN/t4-gate-$g.debug"
    sleep 25
    mark gate "$RUN/t4-gate-$g.log"
    say "  bridge lines in its debug log:"
    grep -aE '\[bridge(:[a-z]+)?\]|\[remote-bridge\]' "$RUN/t4-gate-$g.debug" 2>/dev/null \
      | grep -avE 'Queued|Drained' | cut -c26-200 | head -6 | sed 's/^/    > /'
    say "  .claude.json afterwards: $(jq -c '{remoteDialogSeen, oauthAccount: (.oauthAccount != null), hasCompletedOnboarding}' "$G/.claude.json")"
    say "  alive after 25 s: $(kill -0 "$(panepid gate)" 2>/dev/null && echo yes || echo no)"
    say "  last screen lines:"; screen gate | grep -v '^\s*$' | tail -4 | cut -c1-150 | sed 's/^/    | /'
    CFG="$G" term gate
  done
  ;;
# ---------------------------------------------------------------------------
bg)
  say "=== path 3/4: claude --bg --remote-control, Claude Code's own background session"
  C() { (cd "$WS" && timeout 30 env -i HOME="$SHOME" PATH="$PATH" TERM=dumb LANG="${LANG:-C.UTF-8}" \
          CLAUDE_CONFIG_DIR="$CFG" DISABLE_AUTOUPDATER=1 "$CLAUDE" "$@" </dev/null 2>&1); }
  agents() { C agents --json --all | jq -c '.[] | {id, kind, status, state, pid, sessionId}' 2>/dev/null; }
  out=$(C --bg --remote-control ddspike06bg); say "$out" | clean /dev/stdin | sed 's/^/    | /'
  ID=$(say "$out" | grep -aoE 'backgrounded · [0-9a-f]+' | awk '{print $3}')
  [ -n "$ID" ] || { say "no background id"; exit 1; }
  sleep 8
  say "  claude agents --json:"; agents | sed 's/^/    /'
  WPID=$(C agents --json | jq -r --arg i "$ID" '.[] | select(.id == $i) | .pid')
  DPID=$(ps -o ppid= -p "$(ps -o ppid= -p "$WPID" | tr -d ' ')" | tr -d ' ')
  say "  worker pid $WPID, its PTY host $(ps -o ppid= -p "$WPID" | tr -d ' '), daemon $DPID ($(ps -o args= -p "$DPID" | cut -c1-60)…)"
  C logs "$ID" > "$RUN/bg-logs1.txt"
  B=$(rcline "$RUN/bg-logs1.txt"); U=$(uuid_of "$B")
  say "  Remote Control session from claude logs: $B; registry: $(holders "$U")"
  grep -aE 'proactive refresh|auth:' "$CFG/daemon.log" | tail -2 | cut -c1-120 | sed 's/^/    daemon.log: /'

  say "--- claude attach $ID in a tmux pane, a turn typed there"
  pane a1 "$WS" "$RUN/bg-a1.log" "$CLAUDE" attach "$ID"; sleep 8
  screen a1 | grep -aE 'remote-control is active|/rc' | head -2 | cut -c1-160 | sed 's/^/    | /'
  typeline a1 'Reply with exactly BG1 and nothing else.' 15
  say "  answer: $(screen a1 | grep -aE '^● ' | tail -1)"
  say "--- a second attach at the same time"
  pane a2 "$WS" "$RUN/bg-a2.log" "$CLAUDE" attach "$ID"; sleep 8
  screen a2 | grep -v '^\s*$' | tail -4 | cut -c1-160 | sed 's/^/    a2| /'
  say "  a1 still alive: $(kill -0 "$(panepid a1)" 2>/dev/null && echo yes || echo no)"
  say "--- both attach clients killed (SIGTERM): does the session outlive its viewers?"
  term a2; term a1; sleep 3
  agents | sed 's/^/    /'
  say "--- SIGKILL the worker (a crash): what does the daemon do, and what does attach bring back?"
  kill -KILL "$WPID"; sleep 8
  agents | sed 's/^/    /'
  pane a3 "$WS" "$RUN/bg-a3.log" "$CLAUDE" attach "$ID"; sleep 15
  say "  attach after SIGKILL: session $(rcline "$RUN/bg-a3.log") (was $B)"
  screen a3 | grep -aE 'remote-control|/rc|Error|No job|BG1' | head -4 | cut -c1-160 | sed 's/^/    | /'
  term a3; sleep 3
  say "  after that attach client ended:"; agents | sed 's/^/    /'
  say "--- claude stop $ID, then claude attach $ID again"
  C stop "$ID" | clean /dev/stdin | sed 's/^/    | /'; sleep 5
  agents | sed 's/^/    /'
  pane a4 "$WS" "$RUN/bg-a4.log" "$CLAUDE" attach "$ID"; sleep 15
  say "  after stop + attach: session $(rcline "$RUN/bg-a4.log") (was $B)"
  screen a4 | grep -aE 'remote-control|/rc|Error|No job' | head -3 | cut -c1-160 | sed 's/^/    | /'
  if kill -0 "$(panepid a4)" 2>/dev/null; then
    typeline a4 'What did you reply last time? Answer with that word only.' 15
    say "  conversation carried over? answer: $(screen a4 | grep -aE '^● ' | tail -1)"
  fi
  say "  attach process tree: $(ps -o pid=,args= --ppid "$(panepid a4)" 2>/dev/null | cut -c1-80 | head -2 | tr '\n' ';')"
  term a4; sleep 3
  say "  after that attach client ended:"; agents | sed 's/^/    /'
  say "--- cleanup: stop, rm, wait for the daemon's idle exit"
  C stop "$ID" >/dev/null; C rm "$ID" | clean /dev/stdin | sed 's/^/    | /'
  for _ in $(seq 1 30); do kill -0 "$DPID" 2>/dev/null || break; sleep 1; done
  kill -0 "$DPID" 2>/dev/null && { say "  daemon $DPID still up after 30 s; SIGTERM"; kill -TERM "$DPID"; } || say "  daemon exited on its own"
  tail -3 "$CFG/daemon.log" | cut -c1-140 | sed 's/^/    daemon.log: /'
  ;;
*) say "unknown mode: $MODE"; exit 2 ;;
esac
say "=== done ($MODE)"
