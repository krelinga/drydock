#!/usr/bin/env bash
# Spike 09 — human step H2: the refresh, live, under the split layout, on a
# DISPOSABLE login the owner makes for this run. Never a copy of a real login:
# a refresh rotates the refresh token, and the copied login is then dead.
#
# "Disposable" means a login made in this harness's login container and used
# for nothing else. Best, a separate account. Otherwise a fresh `claude auth
# login` of the owner's own account: that is an OAuth grant of its own (its
# own refresh token), and H2 rotates and finally logs out only that grant.
# The ASSUMPTION, not verified by this spike: rotating or revoking one grant
# leaves the account's other logins (the owner's Drydock, this devcontainer)
# alone. If it does not, H2 signs those out too; recoverable by signing in
# again, but every workspace on that login stops until then.
#
# Usage (from this directory, after ./setup.sh built the dd-spike-09 image):
#   ./h2.sh up        volumes and the login container
#   ./h2.sh login     `claude auth login` in it — the owner completes it in a browser
#   ./h2.sh expire    the disposable login's expiresAt -> now + 6 min (refuses while
#                     any of a, b, c runs: a daemon schedules its refresh at start)
#   ./h2.sh start     a, b: own config volumes, oauthAccount seeded from the login,
#                     `claude --bg --remote-control`; c: the same and a plain
#                     `claude remote-control --spawn worktree --capacity 4 --verbose`
#   ./h2.sh record    every 30 s for RECORD_MIN (10) minutes: the lock entries in the
#                     shared credential volume, the credential's shape (lengths, a
#                     code-point sum of the refresh token that changes on rotation),
#                     each daemon's auth lines, each session's Remote Control id
#   ./h2.sh down      daemon stop --any (a, b), SIGTERM the server (c), `claude auth
#                     logout`, remove the containers and volumes
#   ./h2.sh sweep     after a killed run: stop and remove every container labelled
#                     dd-spike-09-h2=1 (not the volumes; `down` removes those)
#
# H2_FAKE=1 replaces `login` with a fake credential and a fake account (the
# dry run the harness's author did; nothing reaches Remote Control then).
#
# Volumes: dd-s09-h2-cred (the shared credential, CLAUDE_SECURESTORAGE_CONFIG_DIR),
# dd-s09-h2-login (the login's CLAUDE_CONFIG_DIR, where its oauthAccount lands),
# dd-s09-h2-{a,b,c} (each container's own CLAUDE_CONFIG_DIR). Containers carry
# H2's own label, dd-spike-09-h2=1, not run.sh's dd-spike-09=1: run.sh and
# setup.sh sweep theirs at every start, and must never stop a live H2 mid-way.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLAUDE="${CLAUDE_BIN:-$(readlink -f "$(command -v claude)" 2>/dev/null)}"
[ -n "$CLAUDE" ] && [ -x "$CLAUDE" ] && [ ! -d "$CLAUDE" ] \
  || { printf 'REFUSING: no claude binary (not on PATH, and CLAUDE_BIN unset or not executable: %s)\n' "${CLAUDE:-none}"; exit 2; }
LABEL=dd-spike-09-h2=1
P=dd-s09-h2
IN_CFG=/home/vscode/.claude
IN_CRED=/home/vscode/.claude-credentials
say() { printf '%s\n' "$*"; }
running() { docker inspect -f '{{.State.Running}}' "$P-$1" 2>/dev/null | grep -q true; }
ex() { local c="$1"; shift; docker exec -u vscode -w /workspace "$P-$c" "$@"; }
ctr() { # ctr <name> <cfg-volume>
  docker rm -f "$P-$1" >/dev/null 2>&1
  docker run -d --name "$P-$1" --label "$LABEL" --hostname "h2-$1" \
    -v "$P-cred:$IN_CRED" -v "$P-$2:$IN_CFG" -v "$CLAUDE:/usr/local/bin/claude:ro" -v "$HERE:/h:ro" \
    --tmpfs /workspace:uid=1000,gid=1000,mode=755 \
    -e HOME=/home/vscode -e CLAUDE_CONFIG_DIR="$IN_CFG" -e CLAUDE_SECURESTORAGE_CONFIG_DIR="$IN_CRED" \
    -e DISABLE_AUTOUPDATER=1 dd-spike-09 \
    /bin/sh -c 'trap "exit 0" 15; while sleep 1 & wait $!; do :; done' >/dev/null || exit 1
  ex "$1" sh -c 'git init -q && git -c user.email=h2@example.invalid -c user.name=h2 commit -q --allow-empty -m init'
}
case "${1:-}" in
up)
  docker image inspect dd-spike-09 >/dev/null 2>&1 || { say "build the image first: ./setup.sh <scratch>"; exit 1; }
  for v in cred login a b c; do docker volume create "$P-$v" >/dev/null; done
  ctr login login
  say "up: $P-login on $P-cred + $P-login. Next: ./h2.sh login"
  ;;
login)
  running login || { say "run ./h2.sh up first"; exit 1; }
  if [ -n "${H2_FAKE:-}" ]; then
    e=$(( ($(date +%s) + 7200) * 1000 ))
    ex login sh -c "umask 077; printf '%s\n' '{\"claudeAiOauth\":{\"accessToken\":\"sk-ant-oat01-H2-FAKE\",\"refreshToken\":\"sk-ant-ort01-H2-FAKE\",\"expiresAt\":$e,\"scopes\":[\"user:inference\"],\"subscriptionType\":\"max\"}}' > $IN_CRED/.credentials.json"
    ex login sh -c "umask 077; printf '%s\n' '{\"oauthAccount\":{\"accountUuid\":\"00000000-0000-4000-8000-00000000000a\",\"emailAddress\":\"h2@example.invalid\",\"organizationUuid\":\"00000000-0000-4000-8000-0000000000f0\"}}' > $IN_CFG/.claude.json"
    say "H2_FAKE=1: a fake credential and account, no login"
  else
    docker exec -it -u vscode -w /workspace "$P-login" claude auth login --claudeai
  fi
  say "credential: $(ex login jq -c '.claudeAiOauth | {accessToken: (.accessToken|length), refreshToken: (.refreshToken|length), expiresAt}' "$IN_CRED/.credentials.json" 2>&1)"
  say "the login's account record has an organization: $(ex login jq -r '.oauthAccount.organizationUuid != null' "$IN_CFG/.claude.json" 2>&1)"
  ;;
expire)
  # Any running container on the credential volume but the login's, whatever
  # its name or label: a daemon in it scheduled its refresh when it started.
  others=$(docker ps --filter "volume=$P-cred" --format '{{.Names}}') || { say "REFUSING: docker ps failed"; exit 1; }
  others=$(printf '%s\n' "$others" | grep -vx "$P-login" | tr '\n' ' ')
  [ -z "${others// }" ] || { say "REFUSING: $others running on $P-cred; a daemon schedules its refresh when it starts. ./h2.sh down first, or expire before start."; exit 1; }
  running login || { say "run ./h2.sh up first"; exit 1; }
  ex login sh -c "cd $IN_CRED && jq '.claudeAiOauth.expiresAt = ((now + 360) * 1000 | floor)' .credentials.json > .credentials.json.h2 && chmod 600 .credentials.json.h2 && mv .credentials.json.h2 .credentials.json"
  say "expiresAt now $(ex login jq -r '.claudeAiOauth.expiresAt / 1000 | todate' "$IN_CRED/.credentials.json") (about 6 min out). Next: ./h2.sh start, at once"
  ;;
start)
  running login || { say "run ./h2.sh up and login first"; exit 1; }
  # A second start would replace a running a, b or c (ctr removes it with -f,
  # no daemon stop, no SIGTERM) and schedule a second round of refreshes.
  for c in a b c; do running "$c" && { say "REFUSING: $P-$c is running; ./h2.sh down first"; exit 1; }; done
  acct=$(ex login jq -c '.oauthAccount // empty' "$IN_CFG/.claude.json") && [ -n "$acct" ] || { say "the login has no oauthAccount; run ./h2.sh login"; exit 1; }
  for c in a b c; do
    ctr "$c" "$c"
    # the Feature's keys, then the login's oauthAccount under .claude.json.lock (seed-oauth.sh)
    ex "$c" sh -c "umask 077; jq -n '{remoteDialogSeen: true, hasCompletedOnboarding: true, theme: \"dark\", projects: {\"/workspace\": {hasTrustDialogAccepted: true}}}' > $IN_CFG/.claude.json"
    printf '%s\n' "$acct" | docker exec -i -u vscode "$P-$c" sh -c 'umask 077; cat > /tmp/acct.json'
    ex "$c" /h/seed-oauth.sh "$IN_CFG" /tmp/acct.json || exit 1
  done
  for c in a b; do say "$c: $(ex "$c" claude --bg --remote-control "ddspike09h2$c" 2>&1 | grep -aoE 'backgrounded · [0-9a-f]+')"; done
  ex c tmux -L h2 new-session -d -s srv -x 200 -y 50 "claude remote-control --spawn worktree --capacity 4 --verbose 2>&1 | tee /workspace/server.log"
  sleep 15
  for c in a b; do say "$c daemon: $(ex "$c" sh -c "grep -a 'scheduling proactive refresh' $IN_CFG/daemon.log | tail -1")"; done
  say "c server: $(ex c sh -c "grep -aoE 'env_[A-Za-z0-9]+|Capacity: [0-9]+/[0-9]+' /workspace/server.log | sort -u | tr '\n' ' '")"
  say "Next: ./h2.sh record (each daemon's refresh should be scheduled about two minutes out)"
  ;;
record)
  m="${RECORD_MIN:-10}"; case $m in ''|0?*|*[!0123456789]*) say "RECORD_MIN must be a whole number"; exit 2 ;; esac
  for i in $(seq 1 $(( m * 2 ))); do
    say "=== $(date -u +%T)"
    say "  shared cred volume: $(ex a sh -c "ls -A $IN_CRED | tr '\n' ' '")"
    ex a sh -c "[ -f $IN_CRED/.oauth_refresh.lock.owner ] && echo \"  lock owner: \$(cat $IN_CRED/.oauth_refresh.lock.owner)\""
    say "  credential: $(ex a jq -c '.claudeAiOauth | {accessToken: (.accessToken|length), refreshToken: (.refreshToken|length), refreshTokenSum: (.refreshToken | explode | add), expiresAt: (.expiresAt / 1000 | todate)}' "$IN_CRED/.credentials.json" 2>&1)"
    for c in a b; do
      say "  $c daemon.log auth: $(ex "$c" sh -c "grep -a 'auth:' $IN_CFG/daemon.log | tail -3 | cut -c1-140" | tr '\n' '|')"
      say "  $c sessions: $(ex "$c" sh -c "cat $IN_CFG/sessions/*.json 2>/dev/null" | jq -rc 'select(.bridgeSessionId) | .bridgeSessionId' 2>/dev/null | tr '\n' ' ')"
    done
    say "  c server: $(ex c sh -c "grep -aoE 'env_[A-Za-z0-9]+|Capacity: [0-9]+/[0-9]+|[Ee]rror: .{0,80}' /workspace/server.log | sort -u | tail -4 | tr '\n' ' '")"
    sleep 30
  done
  ;;
down)
  for c in a b; do running "$c" && ex "$c" claude daemon stop --any; done
  if running c; then
    pid=$(docker exec "$P-c" ps -eo pid=,args= | awk '/claude remote-control/ && !/tee|tmux/ {print $1; exit}')
    [ -n "$pid" ] && docker exec -u vscode "$P-c" kill -TERM "$pid" && sleep 5
  fi
  running login && ex login claude auth logout
  for c in a b c login; do docker stop -t 10 "$P-$c" >/dev/null 2>&1; docker rm "$P-$c" >/dev/null 2>&1; done
  for v in cred login a b c; do docker volume rm "$P-$v" >/dev/null 2>&1; done
  say "down. Now sign this login out at claude.ai (Settings -> sessions) as well."
  ;;
sweep)
  # By H2's label only; never run.sh's dd-spike-09=1 or anything else.
  ids=$(docker ps -aq --filter "label=$LABEL") || { say "sweep: docker ps failed"; exit 4; }
  [ -n "$ids" ] || exit 0
  # shellcheck disable=SC2086 # a whitespace-separated list of ids
  { docker stop -t 10 $ids >/dev/null; docker rm $ids >/dev/null; }
  left=$(docker ps -aq --filter "label=$LABEL")
  [ -z "$left" ] || { say "sweep: still present: $left"; exit 4; }
  say "sweep: removed the containers labelled $LABEL (the volumes stay; ./h2.sh down removes them)"
  ;;
*) sed -n '2,34p' "$0"; exit 2 ;;
esac
