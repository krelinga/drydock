#!/usr/bin/env bash
# Prepares the shared credential volume each scenario in
# test/drydock/scenarios.json mounts, the way Drydock's §6 step 4 does before any
# workspace mounts one (design §7.1, "Who owns it"): owned by the uid that
# runs the dev container CLI — Drydock's, here the test runner's — 0700, with
# a marker inside so that Docker never copies an image directory's owner into
# it.
#
# Why the scenarios need it: `ubuntu-bare` runs as ubuntu, which is not
# vscode, with the CLI's uid update on. Wherever the runner is not uid 1000
# (GitHub's runners are 1001) the CLI moves ubuntu to the runner's uid but
# leaves the Feature's /home/vscode/.claude at ubuntu's build-time uid, and a
# volume nothing prepared would take that uid on its first mount — exactly
# the failure the preparation exists to prevent, which the Feature's
# preflight then refuses. Prepared, the scenario passes; unprepared, it
# fails on such a runner. So this is part of what the scenario tests.
#
# The script line is container.ownerScript, verbatim; a Go test
# (internal/container) fails when the two differ.
#
# Usage: feature/prepare-test-volumes.sh   (needs docker and jq)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
image=busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e

# BEGIN ownerScript
owner_script='d=/claude; m=$d/.drydock-volume; o=$(stat -c %u "$d") || exit 5; if [ "$o" != "$DRYDOCK_UID" ]; then if [ -n "$(ls -A "$d" | grep -vxF .drydock-volume)" ]; then echo "$o"; exit 4; fi; chown "$DRYDOCK_UID:$DRYDOCK_GID" "$d" && chmod 0700 "$d" || exit 5; fi; if [ ! -e "$m" ] && [ ! -L "$m" ]; then mkdir -m 0700 "$m" 2>/dev/null && chown -h "$DRYDOCK_UID:$DRYDOCK_GID" "$m"; fi; if [ -e "$m" ] || [ -L "$m" ]; then exit 0; fi; exit 5'
# END ownerScript

jq -r '.[].mounts[]? | select(.type == "volume" and .target == "/home/vscode/.claude") | .source' \
	"$here/test/drydock/scenarios.json" | sort -u | while read -r vol; do
	docker volume create "$vol" >/dev/null
	docker run --rm --network none --read-only \
		--cap-drop ALL --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE \
		--security-opt no-new-privileges --user 0:0 \
		--mount "type=volume,source=$vol,target=/claude" \
		--env "DRYDOCK_UID=$(id -u)" --env "DRYDOCK_GID=$(id -g)" \
		--entrypoint sh "$image" -c "$owner_script"
	echo "prepared $vol for uid $(id -u)"
done
