#!/usr/bin/env bash
# Spike 08 — build two throwaway workspaces, one shared CLAUDE_CONFIG_DIR and
# the stand-in container image.
#
# Usage: ./setup.sh <scratch-dir> [source-config-dir]
#
# <scratch>/ws-a, ws-b   git repos with one commit, mounted at /workspace (or,
#                        DISTINCT=1, /workspaces/ws-<w>) in containers A and B
# <scratch>/cfg          the shared CLAUDE_CONFIG_DIR, mounted at
#                        /home/vscode/.claude in both (the Feature's path),
#                        seeded by Spike 02's seed-config.sh: credential,
#                        oauthAccount, remoteDialogSeen, trust for /workspace;
#                        plus hasCompletedOnboarding and theme (Spike 06 result 8)
#
# The real config (default ~/.claude) is only read, never written.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
S="${1:?usage: setup.sh <scratch-dir> [source-config-dir]}"
SRC="${2:-$HOME/.claude}"
# The scratch config is <scratch>/cfg, written and later deleted in: refuse a
# scratch dir that is $HOME or whose cfg is the source (or the real) config.
for real in "$SRC" "$HOME/.claude"; do
  if [ "$(readlink -m -- "$S/cfg")" = "$(readlink -m -- "$real")" ]; then
    echo "REFUSING: $S/cfg is $real, not a scratch copy" >&2; exit 2
  fi
done
[ "$(readlink -m -- "$S")" != "$(readlink -m -- "$HOME")" ] || { echo "REFUSING: the scratch dir is \$HOME" >&2; exit 2; }
# First, before the guard can refuse: whatever a killed run left (sweep.sh).
"$HERE/sweep.sh" || exit 4
# The token guard (Spike 06's): the scratch config gets a copy of the source's
# live refresh token, which a refresh would rotate dead for the source.
# Fails closed: every value passes is_uint (lib.sh has the same, and why)
# before any arithmetic, and every check is `[ ok ] || refuse`, since set -e
# catches neither an arithmetic error nor a `[` that errors inside an `if`.
is_uint() { case $1 in ''|0?*|*[!0123456789]*) return 1 ;; esac; [ "${#1}" -le 15 ]; }
margin="${TOKEN_MARGIN_MIN:-60}"
is_uint "$margin" || { echo "REFUSING: TOKEN_MARGIN_MIN='$margin' is not a whole number of minutes (no leading zero, at most 15 digits)" >&2; exit 3; }
[ "$margin" -ge 30 ] || { echo "REFUSING: TOKEN_MARGIN_MIN=$margin is below the floor of 30" >&2; exit 3; }
exp=$(jq -r '.claudeAiOauth.expiresAt' "$SRC/.credentials.json" 2>/dev/null) || { echo "REFUSING: cannot read expiresAt from $SRC/.credentials.json" >&2; exit 3; }
is_uint "$exp" || { echo "REFUSING: expiresAt ('$exp') is not a whole number of milliseconds (no leading zero, at most 15 digits)" >&2; exit 3; }
now=$(date +%s) && is_uint "$now" || { echo "REFUSING: the clock ('$now') is not a whole number of seconds (no leading zero, at most 15 digits)" >&2; exit 3; }
left=$(( (exp / 1000 - now) / 60 ))
[ "$left" -ge "$margin" ] || {
  echo "REFUSING: $SRC's access token expires in $left min (< $margin);" >&2
  echo "  let the real config refresh first (use claude normally), then re-run." >&2
  exit 3
}
mkdir -p "$S"
S="$(cd "$S" && pwd)"
for w in a b; do
  mkdir -p "$S/ws-$w"
  if [ ! -d "$S/ws-$w/.git" ]; then
    git -C "$S/ws-$w" init -q
    echo "spike 08 $w" > "$S/ws-$w/README"
    git -C "$S/ws-$w" add README
    git -C "$S/ws-$w" -c user.email=spike@example.invalid -c user.name=spike commit -qm init
  fi
done
"$HERE/../harness-02-restart/seed-config.sh" "$S/cfg" "$S/ws-a" "$SRC" >/dev/null
# Trust is keyed by the path the container sees, not the host's: /workspace,
# and /workspaces/ws-<w> for runs with DISTINCT=1 (lib.sh, wsdir).
tmp="$(mktemp "$S/cfg/.claude.json.XXXXXX")"
jq '.projects = {"/workspace": {hasTrustDialogAccepted: true},
                 "/workspaces/ws-a": {hasTrustDialogAccepted: true},
                 "/workspaces/ws-b": {hasTrustDialogAccepted: true}}
    | .hasCompletedOnboarding = true | .theme = "dark"' "$S/cfg/.claude.json" > "$tmp"
chmod 600 "$tmp" && mv "$tmp" "$S/cfg/.claude.json"
jq -c '{org: (.oauthAccount.organizationUuid != null), remoteDialogSeen, hasCompletedOnboarding, trusted: (.projects|keys)}' "$S/cfg/.claude.json"
# A stand-in for the Feature's CLAUDE_ENV_FILE: one line that drops a marker,
# so a session that runs it leaves evidence (the real one is the secrets prelude).
mkdir -p "$S/etc"
printf '%s\n' 'echo "env-file ran pid=$$ DD_MARK=${DD_MARK:-unset}" >> /workspace/.env-file-ran' > "$S/etc/claude-env.sh"
docker build -q -t dd-spike-08 "$HERE" >/dev/null
echo "seeded $S (image dd-spike-08)"
