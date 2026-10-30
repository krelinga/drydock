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
# on the first-run theme picker (result 0 in the report). Drydock's Feature does
# not seed this; the harness does, so every later mode starts at the prompt.
# Set NO_ONBOARDING_SEED=1 to see the picker.
if [ -z "${NO_ONBOARDING_SEED:-}" ]; then
  tmp="$(mktemp "$S/cfg/.claude.json.XXXXXX")"
  jq '.hasCompletedOnboarding = true | .theme = "dark"' "$S/cfg/.claude.json" > "$tmp"
  chmod 600 "$tmp" && mv "$tmp" "$S/cfg/.claude.json"
  echo "  hasCompletedOnboarding: true (harness seed, not the Feature's)"
fi
