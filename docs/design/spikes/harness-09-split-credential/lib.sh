# Spike 09 — shared helpers. Sourced, not run.
#
# The split layout under test: each "workspace" is a scratch container
# (dd-s09-<w>, image dd-spike-09, label dd-spike-09=1) with its OWN
# CLAUDE_CONFIG_DIR (<scratch>/cfg-<name>, mounted at the Feature's
# /home/vscode/.claude), and one credential dir shared by all of them
# (<scratch>/cred for the real-credential modes, <scratch>/fake/cred for the
# fake ones), mounted at /home/vscode/.claude-credentials and named by
# CLAUDE_SECURESTORAGE_CONFIG_DIR. Otherwise run as Spike 08 runs one: PID 1
# the devcontainer CLI's sleep loop, the Feature's DISABLE_AUTOUPDATER=1 and
# CLAUDE_ENV_FILE, HOME, and every command a `docker exec -u vscode` (what
# `devcontainer exec` is), so no variable of the shell running this harness
# reaches anything inside.
#
# The pinned claude is the host's binary, bind-mounted read-only at
# /usr/local/bin/claude, so the version under test is the one run.sh prints.
#
# Cleanup is by recorded name (reap) and by label (sweep.sh). No process outside
# these containers is ever signalled, and no tmux server but this harness's
# private one (-L dd-spike-09) is touched.

S="${S:?set S to the scratch dir setup.sh built}"
CRED="${CRED:-$S/cred}"
RUN="${RUN:-$S/run}"
CLAUDE="${CLAUDE_BIN:-$(readlink -f "$(command -v claude)")}"
TMUX_L=dd-spike-09
CTRS="$S/ctr.names"
BGIDS="$S/bg.ids"
IN_CFG=/home/vscode/.claude
IN_CRED=/home/vscode/.claude-credentials
LABEL=dd-spike-09=1
# Only platform.claude.com carries a refresh (the binary's TOKEN_URL); a
# TEST-NET-3 address drops the SYN, so a refresh's connect hangs and its lock
# stays held long enough to see from outside (Spike 00's blackhole).
BLACKHOLE=(--add-host platform.claude.com:203.0.113.10)

# Nothing here may be the real config: every dir is mounted read-write and
# modes delete in them.
_rp() { readlink -m -- "$1"; }
not_real() { # not_real <dir>: exit unless <dir> is under $S and is not a real config
  local d; d=$(_rp "$1")
  case "$d" in "$(_rp "$S")"/*) ;; *) printf 'REFUSING: %s is not under $S\n' "$1"; exit 2 ;; esac
  for real in "$HOME/.claude" "${CLAUDE_CONFIG_DIR:-$HOME/.claude}"; do
    [ "$d" != "$(_rp "$real")" ] || { printf 'REFUSING: %s is the real config\n' "$1"; exit 2; }
  done
}
[ "$(_rp "$S")" != "$(_rp "$HOME")" ] || { printf 'REFUSING: S is $HOME\n'; exit 2; }
not_real "$CRED"
mkdir -p "$RUN"

say() { printf '%s\n' "$*"; }
t() { tmux -L "$TMUX_L" "$@"; }
now() { date -u +%Y-%m-%dT%H:%M:%S; }

# is_uint <s>: a plain decimal number: ASCII digits only, no leading zero, at
# most 15 digits (Spike 08's, and why).
is_uint() { case $1 in ''|0?*|*[!0123456789]*) return 1 ;; esac; [ "${#1}" -le 15 ]; }

# The token guard (Spike 08's): refuse unless the copied access token's
# expiresAt is still TOKEN_MARGIN_MIN (default 60, at least 30) minutes away
# once the mode has run for the minutes it says it needs. Every --bg daemon and
# remote-control server schedules a proactive refresh before expiresAt; a
# refresh of this copy rotates the owner's real refresh token dead. It covers
# only that scheduled refresh: a 401 on the copy (revoked server-side) would
# send Claude Code to refresh at once — the residual Spikes 06 and 08 carry.
token_guard() { # token_guard <credentials.json> [minutes the mode runs]
  local f="$1" need="${2:-0}" margin="${TOKEN_MARGIN_MIN:-60}" exp left want now
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

ctr() { printf 'dd-s09-%s' "$1"; }
cfgdir() { printf '%s/cfg-%s' "$S" "$1"; }
# wsdir <w>: where the clone is mounted. /workspace for every workspace (Spike
# 08's worst case); with DISTINCT=1, /workspaces/ws-<w>.
wsdir() { if [ -n "${DISTINCT:-}" ]; then printf '/workspaces/ws-%s' "$1"; else printf '/workspace'; fi; }

# mkcfg <name> seeded|bare [creds-file]: a fresh per-container config dir,
# $(cfgdir <name>), from base.claude.json; `seeded` merges the source login's
# oauthAccount into it with seed-oauth.sh (under .claude.json.lock). A
# creds-file (fake modes' NOVAR control only) is installed into it too.
mkcfg() {
  local d; d=$(cfgdir "$1"); not_real "$d"
  rm -rf "$d"; mkdir -p "$d"; chmod 700 "$d"
  install -m 600 "$S/base.claude.json" "$d/.claude.json"
  case "$2" in
    seeded) "$HERE/seed-oauth.sh" "$d" "${ACCT:-$S/oauthAccount.json}" || exit 1 ;;
    bare) ;;
    *) say "mkcfg: $2?"; exit 2 ;;
  esac
  [ -n "${3:-}" ] && install -m 600 "$3" "$d/.credentials.json"
  return 0
}

# ctr_up <w> <cfg-dir> <cred-dir|-> [extra docker run args...]: start
# workspace container <w> on its own config and the shared credential dir
# (CLAUDE_SECURESTORAGE_CONFIG_DIR set); `-` for the cred dir runs it the
# shared-layout way (no variable, no credential mount), the controls' case.
ctr_up() {
  local w="$1" cfg="$2" cred="$3" a=(); shift 3
  not_real "$cfg"
  if [ "$cred" != - ]; then not_real "$cred"; a=(-v "$cred:$IN_CRED" -e CLAUDE_SECURESTORAGE_CONFIG_DIR="$IN_CRED"); fi
  docker rm -f "$(ctr "$w")" >/dev/null 2>&1
  grep -qx "$(ctr "$w")" "$CTRS" 2>/dev/null || printf '%s\n' "$(ctr "$w")" >> "$CTRS"
  mkdir -p "$S/ws-$w"
  [ -d "$S/ws-$w/.git" ] || { git -C "$S/ws-$w" init -q; git -C "$S/ws-$w" -c user.email=spike@example.invalid -c user.name=spike commit -q --allow-empty -m init; }
  docker run -d --name "$(ctr "$w")" --label "$LABEL" --hostname "ws-$w" \
    -v "$cfg:$IN_CFG" "${a[@]}" -v "$S/ws-$w:$(wsdir "$w")" -v "$S/etc:/usr/local/drydock/etc:ro" \
    -v "$CLAUDE:/usr/local/bin/claude:ro" \
    -e HOME=/home/vscode -e CLAUDE_CONFIG_DIR="$IN_CFG" -e DISABLE_AUTOUPDATER=1 \
    -e CLAUDE_ENV_FILE=/usr/local/drydock/etc/claude-env.sh \
    "$@" dd-spike-09 \
    /bin/sh -c 'echo Container started; trap "exit 0" 15; while sleep 1 & wait $!; do :; done' >/dev/null
  sleep 1
  docker inspect -f '{{.State.Running}}' "$(ctr "$w")" 2>/dev/null | grep -q true \
    || { say "container $(ctr "$w") is not running:"; docker logs "$(ctr "$w")" 2>&1 | tail -5; return 1; }
}
# x <w> <cmd...>: a docker exec as vscode in the workspace, stdin closed, bounded.
x() { local w="$1"; shift; timeout "${XT:-30}" docker exec -u vscode -w "$(wsdir "$w")" "$(ctr "$w")" "$@" 2>&1; }
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
agents() { C "$1" agents --json --all | jq -c '.[] | {id, status, state, pid, name, cwd}' 2>/dev/null || C "$1" agents --json --all | head -5; }
ps_in() { docker exec "$(ctr "$1")" ps -eo pid=,ppid=,stat=,args= 2>/dev/null | grep -v ' ps -eo' | cut -c1-150; }
wpid() { C "$1" agents --json --all | jq -r --arg i "$2" '.[] | select(.id == $i) | .pid // empty' 2>/dev/null; }
dpid() { ps_in "$1" | awk '/claude.* daemon run/ {print $1; exit}'; }
osc8ids() { grep -aoE $'\e\\]8;;https://claude\\.ai/code/session_[A-Za-z0-9]+' | grep -aoE 'session_[A-Za-z0-9]+' | sort -u | tr '\n' ' '; }
plainids() { clean | grep -aoE 'claude\.ai/code/session_[A-Za-z0-9]+' | grep -aoE 'session_[A-Za-z0-9]+' | sort -u | tr '\n' ' '; }

# bsid <w> <cfg-dir> <id>: wait up to BSID_WAIT (30) s for job <id>'s worker to
# name its Remote Control session in <cfg-dir>/sessions/<pid>.json; print it
# ("none" if it never does).
bsid() {
  local i p b=
  for i in $(seq 1 "${BSID_WAIT:-30}"); do
    p=$(wpid "$1" "$3")
    [ -n "$p" ] && b=$(jq -r '.bridgeSessionId // empty' "$2/sessions/$p.json" 2>/dev/null)
    [ -n "$b" ] && { printf '%s' "$b"; return 0; }
    sleep 1
  done
  printf 'none'
}
# tree <dir>: every entry under <dir> (depth 2), with type and mode, relative.
tree() { (cd "$1" 2>/dev/null && find . -maxdepth 2 -mindepth 1 -printf '%y %m %P\n' | sort -k3 | grep -v '^. [0-7]* projects/.*\.jsonl$' | tr '\n' ';' | sed 's/;/; /g'); echo; }
# state <cfg-dir>: the daemon state files in one config.
state() {
  ( cd "$1" && say "    jobs/: $(ls jobs 2>/dev/null | grep -v pins.json | tr '\n' ' ')  sessions/: $(ls sessions 2>/dev/null | tr '\n' ' ')"
    [ -f daemon/roster.json ] && say "    daemon/roster.json: $(jq -c '[.workers[]? | {jobId, pid}]' daemon/roster.json 2>/dev/null | cut -c1-300)" )
}
dlog_since() { awk -v t="$2" 'substr($1,2,19) >= t' "$1/daemon.log" 2>/dev/null | cut -c1-200 | sed 's/^/    daemon.log: /'; }
# acct <cfg-dir>: the oauthAccount fields that matter, no personal values:
# whether an organizationUuid is there, profileFetchedAt, and a hash of the rest.
acct() { jq -c '.oauthAccount | if . == null then "none" else {org: (.organizationUuid != null), profileFetchedAt, rest: (del(.profileFetchedAt) | tostring | length)} end' "$1/.claude.json" 2>/dev/null; }
# cred_shape <file>: the credential's shape, no values: token lengths, expiresAt.
cred_shape() { jq -c '.claudeAiOauth | {accessToken: (.accessToken|length), refreshToken: (.refreshToken|length), expiresAt}' "$1" 2>/dev/null || echo "(no credential at $1)"; }

# Fake credentials (Spike 00's): syntactically valid, no real secret. expired
# makes any Claude Code process refresh at once; valid does not.
fake_cred() { # fake_cred <file> expired|valid
  local e
  if [ "$2" = expired ]; then e=$(( ($(date +%s) - 3600) * 1000 )); else e=$(( ($(date +%s) + 7200) * 1000 )); fi
  mkdir -p "$(dirname "$1")"
  printf '{"claudeAiOauth":{"accessToken":"sk-ant-oat01-SPIKE09-FAKE-ACCESS","refreshToken":"sk-ant-ort01-SPIKE09-FAKE-REFRESH","expiresAt":%s,"scopes":["user:inference","user:profile","user:sessions:claude_code"],"subscriptionType":"max"}}\n' "$e" > "$1"
  chmod 600 "$1"
}

# watch_locks <seconds> <out> <dir>...: from the host, every 20 ms, the lock
# entries in each dir (anything named *lock* or .storage-write*), printed when
# the set changes (WATCH_SLEEP=0: as fast as bash can poll), with each .oauth_refresh.lock.owner's pidDomain mapped to
# the container whose pid namespace it is. Run in the background.
watch_locks() {
  local dur="$1" out="$2"; shift 2
  local start el prev="" cur d e line
  declare -A NS=()
  local c ns
  for c in $(cat "$CTRS" 2>/dev/null); do
    ns=$(docker exec "$c" readlink /proc/1/ns/pid 2>/dev/null) || continue  # a container already removed
    [ -n "$ns" ] && NS["$ns"]="$c"
  done
  start=$(date +%s%N); : > "$out"
  while :; do
    el=$(( ($(date +%s%N) - start) / 1000000 )); [ "$el" -le $(( dur * 1000 )) ] || break
    cur=""
    for d in "$@"; do
      for e in "$d"/.oauth_refresh.lock* "$d"/.storage-write* "$d"/*.lock; do
        [ -e "$e" ] || continue
        line="${d#"$S"/}/$(basename "$e")"
        if [ "$(basename "$e")" = .oauth_refresh.lock.owner ]; then
          local dom who="(being written)"; dom=$(jq -r '.pidDomain // "?"' "$e" 2>/dev/null | sed 's/^linux:://')
          [ -n "$dom" ] && who=${NS[$dom]:-$dom}
          line="$line(pid=$(jq -r .pid "$e" 2>/dev/null) in $who)"
        else line="$line(ino=$(stat -c %i "$e" 2>/dev/null))"; fi
        cur="$cur $line"
      done
    done
    [ "$cur" != "$prev" ] && { printf '%6d ms %s\n' "$el" "${cur:- (none)}" >> "$out"; prev="$cur"; }
    [ "${WATCH_SLEEP:-0.02}" = 0 ] || sleep "${WATCH_SLEEP:-0.02}"
  done
  printf '%6d ms (watch ended)\n' "$el" >> "$out"
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

# reap: this harness's tmux server; `claude stop` each recorded background id
# in its own container (graceful: archives the session); then docker stop and
# remove each recorded container. Nothing not recorded is touched.
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
