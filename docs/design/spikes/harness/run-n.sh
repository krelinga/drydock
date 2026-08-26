#!/bin/bash
# Usage: run-n.sh <N> <tag> [--blackhole]
BIN="${CLAUDE_BIN:-$(readlink -f "$(command -v claude)")}"
S="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "claude under test: $("$BIN" --version 2>&1 | head -1)"
VOL=/var/lib/docker/volumes/dd-spike-creds/_data
docker volume create dd-spike-creds >/dev/null
N=$1; TAG=$2; BH=$3
docker rm -f $(docker ps -aq --filter "name=dd-spike-") >/dev/null 2>&1
docker run --rm -v dd-spike-creds:/cfg -v "$S":/spike:ro debian:bookworm-slim /spike/seed.sh
sudo rm -f $VOL/dbg-*.log $VOL/out-*.txt

HOSTARG=""
[ "$BH" = "--blackhole" ] && HOSTARG="--add-host platform.claude.com:203.0.113.10"

sudo bash "$S/watch.sh" "$VOL" 40 > "$S/watch-$TAG.txt" 2>&1 &
WPID=$!
sleep 1
for i in $(seq 1 $N); do
  docker run --rm --name dd-spike-$i $HOSTARG \
    -v "$BIN":/usr/local/bin/claude:ro -v dd-spike-creds:/cfg \
    -e CLAUDE_CONFIG_DIR=/cfg -e HOME=/root debian:bookworm-slim \
    timeout 35 /usr/local/bin/claude -p hi --debug-file /cfg/dbg-$i.log >/dev/null 2>&1 &
done
wait $WPID
docker rm -f $(docker ps -aq --filter "name=dd-spike-") >/dev/null 2>&1
echo "=== TIMELINE $TAG ==="
cat "$S/watch-$TAG.txt"
