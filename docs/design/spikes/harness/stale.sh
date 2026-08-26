#!/bin/bash
# Experiment 3: a container dies holding the refresh lock. Does the fleet wedge, or self-heal?
# proper-lockfile semantics: lockfilePath is a DIRECTORY; stale=60000ms, heartbeat update=5000ms.
BIN="${CLAUDE_BIN:-$(readlink -f "$(command -v claude)")}"
S="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "claude under test: $("$BIN" --version 2>&1 | head -1)"
VOL=/var/lib/docker/volumes/dd-spike-creds/_data
docker volume create dd-spike-creds >/dev/null
AGE=$1   # seconds of staleness to simulate

docker rm -f $(docker ps -aq --filter "name=dd-spike-") >/dev/null 2>&1
docker run --rm -v dd-spike-creds:/cfg -v "$S":/spike:ro debian:bookworm-slim /spike/seed.sh >/dev/null

# Simulate an abandoned lock: create the lock dir and backdate its mtime.
sudo rm -rf "$VOL/.oauth_refresh.lock"
sudo mkdir -p "$VOL/.oauth_refresh.lock"
sudo touch -d "@$(( $(date +%s) - AGE ))" "$VOL/.oauth_refresh.lock"
echo "planted abandoned lock, mtime age = ${AGE}s (stale threshold = 60s)"

start=$(date +%s%N)
docker run --rm --name dd-spike-solo \
  -v "$BIN":/usr/local/bin/claude:ro -v dd-spike-creds:/cfg \
  -e CLAUDE_CONFIG_DIR=/cfg -e HOME=/root debian:bookworm-slim \
  timeout 60 /usr/local/bin/claude -p hi >"$S/stale-$AGE.out" 2>&1
rc=$?
end=$(date +%s%N)
echo "exit=$rc  elapsed=$(( (end-start)/1000000 ))ms"
echo "--- stdout ---"; cat "$S/stale-$AGE.out"
echo "--- lock still present? ---"; sudo ls -lad "$VOL/.oauth_refresh.lock" 2>&1
echo "--- credential now ---"; sudo cat "$VOL/.credentials.json"; echo
