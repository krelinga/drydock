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
image=
# The image goes after the container: Docker refuses to remove an image a
# container still uses. A refusal is reported and fails the run, never
# swallowed.
cleanup() {
	status=$?
	ids=$(docker ps -aq --filter "label=$label")
	[ -z "$ids" ] || docker rm -f $ids >/dev/null
	if [ -n "$image" ] && ! docker image rm -- "$image" >/dev/null; then
		echo "measure-image-names.sh: could not remove image $image; remove it by hand" >&2
		status=1
	fi
	rm -rf /tmp/drydock-measure
	exit "$status"
}
trap cleanup EXIT
(cd / && devcontainer up --workspace-folder "$folder" --id-label "$label" >/dev/null 2>&1)
image=$(docker inspect --format '{{.Config.Image}}' "$(docker ps -aq --filter "label=$label")")
printf 'cli %s\nfolder %s\nimage %s\n' "$(devcontainer --version)" "$folder" "$image"
