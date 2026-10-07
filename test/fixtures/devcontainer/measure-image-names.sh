#!/usr/bin/env bash
# Re-measures the names `devcontainer up` gives the images it builds for a
# workspace folder (container.BuiltImages, image-names.txt beside this), run
# from / so the process's working directory differs from the folder. Needs
# Docker, the devcontainer CLI and alpine:3.20; leaves nothing behind.
#
#   test/fixtures/devcontainer/measure-image-names.sh > test/fixtures/devcontainer/image-names.txt
set -euo pipefail
folder=/tmp/drydock-measure/ws/01JABCDEFGHJKMNPQRSTVWXYZ0/repo
label="drydock.measure=$$"
rm -rf /tmp/drydock-measure
mkdir -p "$folder/.devcontainer/f"
printf '{"image":"alpine:3.20","features":{"./f":{}}}' >"$folder/.devcontainer/devcontainer.json"
printf '{"id":"f","version":"1.0.0","name":"f"}' >"$folder/.devcontainer/f/devcontainer-feature.json"
printf '#!/bin/sh\ntrue\n' >"$folder/.devcontainer/f/install.sh"
chmod +x "$folder/.devcontainer/f/install.sh"
cleanup() {
	ids=$(docker ps -aq --filter "label=$label")
	[ -z "$ids" ] || docker rm -f $ids >/dev/null
	rm -rf /tmp/drydock-measure
}
trap cleanup EXIT
(cd / && devcontainer up --workspace-folder "$folder" --id-label "$label" >/dev/null 2>&1)
image=$(docker inspect --format '{{.Config.Image}}' "$(docker ps -aq --filter "label=$label")")
printf 'cli %s\nfolder %s\nimage %s\n' "$(devcontainer --version)" "$folder" "$image"
docker image rm -- "$image" >/dev/null 2>&1 || true
