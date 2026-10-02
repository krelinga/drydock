#!/usr/bin/env bash
# What does `claude auth status --json` report for each credential state the
# expiry watch (design §7.3) has to tell apart?
#
# Usage: ./auth-status.sh
#
# Builds four throwaway CLAUDE_CONFIG_DIRs with synthetic credentials and asks
# `claude auth status --json` about each. All tokens are fake; nothing here can
# authenticate, which is fine because the question is what the command *reports*,
# not whether the token works. The real credential is never read or written.
set -uo pipefail

BIN="${CLAUDE_BIN:-$(readlink -f "$(command -v claude)")}"
RUN="${DRYDOCK_SPIKE_RUN:-$(mktemp -d -t drydock-spike-01-auth-XXXXXX)}"
IMG=debian:bookworm-slim

# A plausible account record, so the absence of one is never the reason a state
# reports as signed out.
ACCOUNT='{"accountUuid":"00000000-0000-0000-0000-000000000000",
          "emailAddress":"spike@example.invalid",
          "organizationUuid":"11111111-1111-1111-1111-111111111111",
          "organizationName":"spike-org","subscriptionType":"max"}'

mk() { # mk <name> <credentials-json-or-empty>
  local name="$1"
  local cred="$2"
  local dir="$RUN/$name"
  mkdir -p "$dir"; chmod 777 "$dir"
  [ -n "$cred" ] && { printf '%s' "$cred" > "$dir/.credentials.json"; chmod 666 "$dir/.credentials.json"; }
  jq -n --argjson a "$ACCOUNT" '{oauthAccount:$a}' > "$dir/.claude.json"
  chmod 666 "$dir/.claude.json"
}

ask() { # ask <name>
  local name="$1"
  local raw parsed
  raw=$(docker run --rm -v "$BIN":/usr/local/bin/claude:ro -v "$RUN/$name":/cfg \
          -e CLAUDE_CONFIG_DIR=/cfg -e HOME=/root "$IMG" \
          /usr/local/bin/claude auth status --json 2>/dev/null)
  parsed=$(jq -c '{loggedIn, authMethod, subscriptionType}' <<<"$raw" 2>/dev/null)
  printf '%-22s %s\n' "$name" "${parsed:-<no parseable output>}"
}

FUTURE=$(( ($(date +%s) + 86400 * 30) * 1000 ))
PAST=$(( ($(date +%s) - 3600) * 1000 ))

mk no-credential ''
mk valid    "{\"claudeAiOauth\":{\"accessToken\":\"sk-ant-oat01-SPIKE-FAKE\",\"refreshToken\":\"sk-ant-ort01-SPIKE-FAKE\",\"expiresAt\":$FUTURE,\"scopes\":[\"user:inference\",\"user:profile\"],\"subscriptionType\":\"max\"}}"
mk expired  "{\"claudeAiOauth\":{\"accessToken\":\"sk-ant-oat01-SPIKE-FAKE\",\"refreshToken\":\"sk-ant-ort01-SPIKE-FAKE\",\"expiresAt\":$PAST,\"scopes\":[\"user:inference\",\"user:profile\"],\"subscriptionType\":\"max\"}}"
# The tombstone from Spike 00: a dead login is blanked in place, not deleted.
mk blanked  "{\"claudeAiOauth\":{\"accessToken\":\"\",\"refreshToken\":\"\",\"expiresAt\":0,\"scopes\":[\"user:inference\",\"user:profile\"],\"subscriptionType\":\"max\"}}"

echo "claude under test: $("$BIN" --version 2>&1 | head -1)"
echo "artifacts=$RUN"
echo
echo "state                  auth status --json"
for s in no-credential valid expired blanked; do ask "$s"; done
echo
echo "expiresAt is NOT reported by auth status; read it from"
echo ".credentials.json -> claudeAiOauth.expiresAt (see Spike 00 for write semantics)."
