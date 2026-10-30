#!/usr/bin/env bash
# Spike 06 — build a throwaway workspace and CLAUDE_CONFIG_DIR.
#
# Usage: ./setup.sh <scratch-dir> [source-config-dir]
#
# <scratch-dir>/ws   a git repo with one commit (worktree mode needs one)
# <scratch-dir>/cfg  a CLAUDE_CONFIG_DIR seeded by Spike 02's seed-config.sh:
#                    credential, oauthAccount, remoteDialogSeen, trust for ws
#
# The real config (default ~/.claude) is only read, never written.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
S="${1:?usage: setup.sh <scratch-dir> [source-config-dir]}"
SRC="${2:-$HOME/.claude}"
# The scratch config gets a copy of the source's live refresh token, which can
# refresh on its own and rotate the source's login dead (see README, traps).
# Refuse a copy whose access token is near the point Claude Code refreshes it.
margin="${TOKEN_MARGIN_MIN:-60}"
case "$margin" in ''|*[!0-9]*|0?*|????????????????*) echo "REFUSING: TOKEN_MARGIN_MIN='$margin' is not a whole number of minutes (no leading zero, at most 15 digits)" >&2; exit 3 ;; esac
[ "$((10#$margin))" -ge 30 ] || { echo "REFUSING: TOKEN_MARGIN_MIN=$margin is below the floor of 30" >&2; exit 3; }
exp=$(jq -r '.claudeAiOauth.expiresAt' "$SRC/.credentials.json" 2>/dev/null) || { echo "REFUSING: cannot read expiresAt from $SRC/.credentials.json" >&2; exit 3; }
case "$exp" in ''|*[!0-9]*|0?*|????????????????*) echo "REFUSING: expiresAt ('$exp') is not a whole number of milliseconds (no leading zero, at most 15 digits)" >&2; exit 3 ;; esac
now=$(date +%s) || exit 3
left=$(echo $(( (10#$exp / 1000 - 10#$now) / 60 )) 2>/dev/null)  # a failed expansion leaves $left empty; the case below refuses on it
case "$left" in ""|*[!0-9-]*|-) echo "REFUSING: arithmetic on the expiry failed" >&2; exit 3 ;; esac
if [ "$left" -lt "$margin" ]; then
  echo "REFUSING: $SRC's access token expires in $left min (< $margin);" >&2
  echo "  let the real config refresh first (use claude normally), then re-run." >&2
  exit 3
fi
mkdir -p "$S/ws"
S="$(cd "$S" && pwd)"
if [ ! -d "$S/ws/.git" ]; then
  git -C "$S/ws" init -q
  echo "spike 06" > "$S/ws/README"
  git -C "$S/ws" add README
  git -C "$S/ws" -c user.email=spike@example.invalid -c user.name=spike commit -qm init
fi
"$HERE/../harness-02-restart/seed-config.sh" "$S/cfg" "$S/ws" "$SRC"

# An *interactive* claude on a config that has only served Remote Control opens
# on the first-run theme picker (result 8 in the report). Drydock's Feature does
# not seed this; the harness does, so every later mode starts at the prompt.
# Set NO_ONBOARDING_SEED=1 to see the picker.
if [ -z "${NO_ONBOARDING_SEED:-}" ]; then
  tmp="$(mktemp "$S/cfg/.claude.json.XXXXXX")"
  jq '.hasCompletedOnboarding = true | .theme = "dark"' "$S/cfg/.claude.json" > "$tmp"
  chmod 600 "$tmp" && mv "$tmp" "$S/cfg/.claude.json"
  echo "  hasCompletedOnboarding: true (harness seed, not the Feature's)"
fi
