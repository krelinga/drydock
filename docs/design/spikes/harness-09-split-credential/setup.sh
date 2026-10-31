#!/usr/bin/env bash
# Spike 09 — build the split layout's scratch state and the stand-in image.
#
# Usage: ./setup.sh <scratch-dir> [source-config-dir]
#
# <scratch>/ws-a, ws-b       git repos with one commit, mounted at /workspace (or,
#                            DISTINCT=1, /workspaces/ws-<w>) in containers A and B
# <scratch>/cred             THE SHARED CREDENTIAL DIR: a copy of the source's
#                            .credentials.json and nothing else, mounted in every
#                            container at /home/vscode/.claude-credentials, which
#                            CLAUDE_SECURESTORAGE_CONFIG_DIR names
# <scratch>/oauthAccount.json  the source login's oauthAccount object, alone
# <scratch>/base.claude.json   what every per-container .claude.json starts from:
#                            remoteDialogSeen, trust for /workspace,
#                            /workspaces/ws-a, /workspaces/ws-b, and Spike 06's
#                            onboarding seed (hasCompletedOnboarding, theme).
#                            No oauthAccount: run.sh's mkcfg seeds it (or not)
#                            into each container's own <scratch>/cfg-<name>
# <scratch>/etc/claude-env.sh  a stand-in CLAUDE_ENV_FILE (drops a marker line)
#
# The real config (default ~/.claude) is only read, never written. The
# fake-credential modes (lock, blank, bump) build their own state under
# <scratch>/fake and never mount <scratch>/cred.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
S="${1:?usage: setup.sh <scratch-dir> [source-config-dir]}"
SRC="${2:-$HOME/.claude}"
# Everything under <scratch> is written and later deleted in: refuse a scratch
# dir that is $HOME, or one whose cred dir is the source (or the real) config.
for real in "$SRC" "$HOME/.claude" "${CLAUDE_CONFIG_DIR:-$HOME/.claude}"; do
  for d in "$S" "$S/cred"; do
    if [ "$(readlink -m -- "$d")" = "$(readlink -m -- "$real")" ]; then
      echo "REFUSING: $d is $real, not a scratch copy" >&2; exit 2
    fi
  done
done
[ "$(readlink -m -- "$S")" != "$(readlink -m -- "$HOME")" ] || { echo "REFUSING: the scratch dir is \$HOME" >&2; exit 2; }
# First, before the guard can refuse: whatever a killed run left (sweep.sh).
"$HERE/sweep.sh" || exit 4
# The token guard (Spike 06's, as Spike 08 has it): <scratch>/cred gets a copy
# of the source's live refresh token, which a refresh would rotate dead for the
# source. Fails closed: every value passes is_uint before any arithmetic, and
# every check is `[ ok ] || refuse`.
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
umask 077
for w in a b c; do
  mkdir -p "$S/ws-$w"
  if [ ! -d "$S/ws-$w/.git" ]; then
    git -C "$S/ws-$w" init -q
    echo "spike 09 $w" > "$S/ws-$w/README"
    git -C "$S/ws-$w" add README
    git -C "$S/ws-$w" -c user.email=spike@example.invalid -c user.name=spike commit -qm init
  fi
done
# The shared credential dir holds the credential and nothing else.
mkdir -p "$S/cred"; chmod 700 "$S/cred"
install -m 600 "$SRC/.credentials.json" "$S/cred/.credentials.json"
# The one object the split layout seeds into each container's config.
jq -c '.oauthAccount // empty' "$SRC/.claude.json" > "$S/oauthAccount.json"
chmod 600 "$S/oauthAccount.json"
jq -e '(.organizationUuid // "") != ""' "$S/oauthAccount.json" >/dev/null \
  || { echo "REFUSING: $SRC/.claude.json has no oauthAccount.organizationUuid; sign in first" >&2; exit 1; }
# Every per-container .claude.json starts from this: the Feature's two keys
# (trust, remoteDialogSeen) and Spike 06's onboarding seed. No oauthAccount.
jq -n '{remoteDialogSeen: true, hasCompletedOnboarding: true, theme: "dark",
        projects: {"/workspace": {hasTrustDialogAccepted: true},
                   "/workspaces/ws-a": {hasTrustDialogAccepted: true},
                   "/workspaces/ws-b": {hasTrustDialogAccepted: true},
                   "/workspaces/ws-c": {hasTrustDialogAccepted: true}}}' > "$S/base.claude.json"
# A stand-in for the Feature's CLAUDE_ENV_FILE: one line that drops a marker.
mkdir -p "$S/etc"; chmod 755 "$S/etc"
printf '%s\n' 'echo "env-file ran pid=$$" >> "$PWD/.env-file-ran"' > "$S/etc/claude-env.sh"
chmod 644 "$S/etc/claude-env.sh"
docker build -q -t dd-spike-09 "$HERE" >/dev/null
echo "seeded $S (image dd-spike-09): cred $(ls -A "$S/cred" | tr '\n' ' '), oauthAccount has org: $(jq -r '.organizationUuid != null' "$S/oauthAccount.json"), token ${left} min left"
