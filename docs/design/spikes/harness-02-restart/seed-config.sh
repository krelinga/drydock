#!/usr/bin/env bash
# Everything a Remote Control server needs in its CLAUDE_CONFIG_DIR before it
# will start without a human at the keyboard. This is the mechanical equivalent
# of what §7.2 describes as "run `claude` once in /workspace to clear the trust
# dialog" — plus two gates that section does not mention.
#
# Usage: ./seed-config.sh <config-dir> <workspace-dir> [source-config-dir]
#
# Gate 1  workspace trust      projects["<dir>"].hasTrustDialogAccepted = true
# Gate 2  Remote Control consent   remoteDialogSeen = true
# Gate 3  account/organization record   oauthAccount (NOT in .credentials.json)
#
# Gate 3 is the one that breaks the obvious design: a credential file alone is
# not enough for Remote Control. Without `oauthAccount.organizationUuid` the
# server exits with "Unable to determine your organization for Remote Control
# eligibility", even though the token is valid and makes model requests fine.
# Sharing the whole CLAUDE_CONFIG_DIR (design §7.1) carries it; copying just
# .credentials.json does not.
set -euo pipefail

CFG="${1:?usage: seed-config.sh <config-dir> <workspace-dir> [source-config-dir]}"
WS="${2:?usage: seed-config.sh <config-dir> <workspace-dir> [source-config-dir]}"
SRC="${3:-$HOME/.claude}"

mkdir -p "$CFG"
WS="$(cd "$WS" && pwd)"

if [ ! -f "$SRC/.credentials.json" ]; then
  echo "no credential at $SRC/.credentials.json — run 'claude auth login' first" >&2
  exit 1
fi
install -m 600 "$SRC/.credentials.json" "$CFG/.credentials.json"

# Carry the account record across, then set both dialog gates. Written as one
# jq pass so a partial failure leaves no half-seeded config.
jq -n \
  --argjson oauth "$(jq -c '.oauthAccount // {}' "$SRC/.claude.json")" \
  --arg uid "$(jq -r '.userID // ""' "$SRC/.claude.json")" \
  --arg ws "$WS" \
  '{
     oauthAccount: $oauth,
     userID: $uid,
     remoteDialogSeen: true,
     projects: { ($ws): { hasTrustDialogAccepted: true } }
   }' > "$CFG/.claude.json"
chmod 600 "$CFG/.claude.json"

echo "seeded $CFG for workspace $WS"
jq -r '"  oauthAccount.organizationUuid: " +
       (if (.oauthAccount.organizationUuid // "") == "" then "MISSING — Remote Control will refuse"
        else "present" end),
       "  remoteDialogSeen: \(.remoteDialogSeen)",
       "  trusted: \(.projects | keys | join(", "))"' "$CFG/.claude.json"
