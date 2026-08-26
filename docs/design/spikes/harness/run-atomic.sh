#!/bin/bash
BIN="${CLAUDE_BIN:-$(readlink -f "$(command -v claude)")}"
S="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "claude under test: $("$BIN" --version 2>&1 | head -1)"
VOL=/var/lib/docker/volumes/dd-spike-creds/_data
docker volume create dd-spike-creds >/dev/null
docker rm -f $(docker ps -aq --filter "name=dd-spike-") >/dev/null 2>&1
sudo rm -rf "$VOL/.oauth_refresh.lock"
docker run --rm -v dd-spike-creds:/cfg -v "$S":/spike:ro debian:bookworm-slim /spike/seed.sh
sudo bash "$S/atomic.sh" "$VOL" 25 &
AP=$!
sleep 1
# 3 containers all racing to refresh against the real endpoint -> real writes to the shared file
for i in 1 2 3; do
  docker run --rm --name dd-spike-$i \
    -v "$BIN":/usr/local/bin/claude:ro -v dd-spike-creds:/cfg \
    -e CLAUDE_CONFIG_DIR=/cfg -e HOME=/root debian:bookworm-slim \
    timeout 20 /usr/local/bin/claude -p hi >/dev/null 2>&1 &
done
wait $AP
docker rm -f $(docker ps -aq --filter "name=dd-spike-") >/dev/null 2>&1
echo ATOMIC_DONE
