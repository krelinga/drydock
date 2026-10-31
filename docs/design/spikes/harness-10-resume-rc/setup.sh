#!/usr/bin/env bash
# Spike 10 — build a throwaway workspace and CLAUDE_CONFIG_DIR.
#
# Usage: ./setup.sh <scratch-dir> [source-config-dir]
#
# Spike 06's setup.sh, unchanged: <scratch>/ws (a git repo with one commit) and
# <scratch>/cfg (seeded like the shared volume: credential, oauthAccount,
# remoteDialogSeen, trust for ws, plus hasCompletedOnboarding). It carries the
# fail-closed token guard (is_uint before any arithmetic) and refuses a copy
# whose access token is less than TOKEN_MARGIN_MIN (default 60) minutes from
# expiry. The source config is only read.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$HERE/../harness-06-attach/setup.sh" "$@"
