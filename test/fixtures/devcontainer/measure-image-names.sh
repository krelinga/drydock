#!/usr/bin/env bash
# Re-measures the names `devcontainer up` gives the images it builds for a
# workspace folder (container.BuiltImages, image-names.txt beside this), and
# the workspace folder it reports in the container, for clone folders named as
# Drydock names them — after the repository, in its own case
# (workspace.CloneFolder): a plain name, one with capitals, dots and an
# underscore, and one whose separators the CLI collapses. Each is run from /
# so the process's working directory differs from the folder. Needs Docker,
# the devcontainer CLI, jq and alpine:3.20; leaves nothing behind.
#
#   test/fixtures/devcontainer/measure-image-names.sh > test/fixtures/devcontainer/image-names.txt
set -euo pipefail
root=/tmp/drydock-measure/ws/01JABCDEFGHJKMNPQRSTVWXYZ0
label="drydock.measure=$$"
images=()
# The images go after the containers: Docker refuses to remove an image a
# container still uses. A run that failed before reading an image takes its
# name from the labelled container if there is one; with no container either,
# it says an image may be left. A refusal is reported and fails the run,
# never swallowed.
cleanup() {
	status=$?
	ids=$(docker ps -aq --filter "label=$label")
	if [ -n "$ids" ]; then
		while read -r i; do images+=("$i"); done < <(docker inspect --format '{{.Config.Image}}' $ids)
		docker rm -f $ids >/dev/null
	fi
	for image in $(printf '%s\n' "${images[@]}" | sort -u); do
		if ! docker image rm -- "$image" >/dev/null; then
			echo "measure-image-names.sh: could not remove image $image; remove it by hand" >&2
			status=1
		fi
	done
	if [ "${#images[@]}" -eq 0 ] && [ "$status" -ne 0 ]; then
		echo "measure-image-names.sh: failed before any container was made; an image devcontainer up built (vsc-*-features) may be left" >&2
	fi
	rm -rf /tmp/drydock-measure
	exit "$status"
}
trap cleanup EXIT
rm -rf /tmp/drydock-measure
printf 'cli %s\n' "$(devcontainer --version)"
for name in alpha Drydock.Testbed_A a-_b..c; do
	folder=$root/$name
	mkdir -p "$folder/.devcontainer/f"
	printf '{"image":"alpine:3.20","features":{"./f":{}}}' >"$folder/.devcontainer/devcontainer.json"
	printf '{"id":"f","version":"1.0.0","name":"f"}' >"$folder/.devcontainer/f/devcontainer-feature.json"
	printf '#!/bin/sh\ntrue\n' >"$folder/.devcontainer/f/install.sh"
	chmod +x "$folder/.devcontainer/f/install.sh"
	out=$(cd / && devcontainer up --workspace-folder "$folder" --id-label "$label" 2>/dev/null)
	id=$(jq -r .containerId <<<"$out")
	image=$(docker inspect --format '{{.Config.Image}}' "$id")
	images+=("$image")
	printf 'image %s %s\n' "$folder" "$image"
	printf 'workspace-folder %s %s\n' "$folder" "$(jq -r .remoteWorkspaceFolder <<<"$out")"
	docker rm -f "$id" >/dev/null
done
