#!/usr/bin/env bash
# Spike 10 — build a throwaway workspace and CLAUDE_CONFIG_DIR.
#
# Usage: ./setup.sh <scratch-dir> [source-config-dir]
#
# Spike 06's setup.sh: <scratch>/ws (a git repo with one commit) and
# <scratch>/cfg (seeded like the shared volume: credential, oauthAccount,
# remoteDialogSeen, trust for ws, plus hasCompletedOnboarding). It carries the
# fail-closed token guard (is_uint before any arithmetic) and refuses a copy
# whose access token is less than TOKEN_MARGIN_MIN (default 60) minutes from
# expiry. The source config is only read.
#
# First, lib.sh's scratch refusal: <scratch> must not be $HOME, and neither it
# nor <scratch>/cfg nor <scratch>/home may be, be inside, or contain the real
# config (~/.claude, $CLAUDE_CONFIG_DIR), since seeding writes <scratch>/cfg.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
S="${1:?usage: setup.sh <scratch-dir> [source-config-dir]}"
_rp() { readlink -m -- "$1"; }
_overlaps() {
  local a b; a=$(_rp "$1"); b=$(_rp "$2")
  [ "$a" = "$b" ] && return 0
  case "$a/" in "$b"/*) return 0 ;; esac
  case "$b/" in "$a"/*) return 0 ;; esac
  return 1
}
for real in "$HOME/.claude" ${CLAUDE_CONFIG_DIR:+"$CLAUDE_CONFIG_DIR"}; do
  for p in "$S/cfg" "$S" "$S/home"; do
    if _overlaps "$p" "$real"; then echo "REFUSING: $p overlaps the real config $real" >&2; exit 2; fi
  done
done
if [ "$(_rp "$S")" = "$(_rp "$HOME")" ]; then echo "REFUSING: $S is \$HOME, not a scratch dir" >&2; exit 2; fi
exec "$HERE/../harness-06-attach/setup.sh" "$@"
