#!/bin/sh
# Seed an expired, syntactically-valid but FAKE credential into the shared volume.
# No real secret is involved: the refresh will fail at the server with invalid_grant,
# but the lock-acquisition path we are measuring runs *before* that HTTP call.
set -e
PAST=$(( ($(date +%s) - 3600) * 1000 ))
cat > /cfg/.credentials.json <<JSON
{"claudeAiOauth":{
  "accessToken":"sk-ant-oat01-SPIKE-FAKE-ACCESS-TOKEN-000",
  "refreshToken":"sk-ant-ort01-SPIKE-FAKE-REFRESH-TOKEN-000",
  "expiresAt":$PAST,
  "scopes":["user:inference","user:profile"],
  "subscriptionType":"max"
}}
JSON
chmod 600 /cfg/.credentials.json
echo "seeded, expiresAt=$PAST ($(date -d @$((PAST/1000)) -u 2>/dev/null || true))"
