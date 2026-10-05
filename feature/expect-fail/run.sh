#!/usr/bin/env bash
# Builds the Feature must fail (testing §6.5): real `devcontainer up` runs of
# the Feature from this checkout, each expected to fail, naming why.
#
# Why this is not in feature/test/drydock/scenarios.json: the official harness
# (`devcontainer features test`, CLI 0.89.0) has no notion of a scenario that
# should fail to build. A container that does not come up is a fatal
# "Failed to launch container" that ends the whole run (process.exit(1) in
# the CLI's test command), so a scenario that is meant to fail cannot be
# expressed there, and one that silently succeeded would only look like a
# harness problem. The scenarios there run drydock-preflight directly for the
# same five variables; this runs the real build, so what is asserted is that
# the Feature's postCreateCommand fails `devcontainer up` itself — which is
# what a Drydock workspace would see — and says which variable.
#
# Each case is a devcontainer.json and the text the failed build must print
# (alternatives separated by ";;").
# The positive control is the first case: the same configuration with
# nothing wrong must come up. Without it, a runner that could not build
# anything would pass every case.
#
# Usage: feature/expect-fail/run.sh [case-name-substring]
# Needs docker and the devcontainer CLI. Leaves nothing behind.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
src=$here/../src/drydock
filter=${1:-}
run=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
label=drydock-expect-fail=$run
vol=drydock-expect-fail-$run
vol2=drydock-expect-fail-$run-other
image=mcr.microsoft.com/devcontainers/base:debian
work=$(mktemp -d)

cleanup() {
	ids=$(docker ps -aq --filter "label=$label")
	# shellcheck disable=SC2086 # one id per word
	[ -z "$ids" ] || docker rm -f $ids >/dev/null
	docker volume rm -f "$vol" "$vol2" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

mount() { printf '{"type":"volume","source":"%s","target":"%s"}' "$1" "$2"; }

# name | expected ("ok" for success) | extra devcontainer.json members.
#
# The shared volume is passed as Drydock passes it — `up --mount`, not the
# configuration's "mounts" — in every case but "no shared volume". That
# matters for the same-path case. Measured on CLI 0.89.0: two entries at one
# target inside the configuration's own "mounts" are merged by the CLI and one
# silently wins, with no error at all; but a --mount from the command line is
# never merged with them, so Docker gets both and refuses the container
# ("Duplicate mount point"). So under Drydock a repository cannot displace the
# shared volume at its path, and a container that has some other volume there
# is one Drydock did not make.
cases=(
	"baseline|ok|"
	"ANTHROPIC_BASE_URL|drydock feature: ANTHROPIC_BASE_URL is set|\"containerEnv\":{\"ANTHROPIC_BASE_URL\":\"https://gateway.example.com\"}"
	"DISABLE_TELEMETRY|drydock feature: DISABLE_TELEMETRY is set|\"containerEnv\":{\"DISABLE_TELEMETRY\":\"1\"}"
	"DO_NOT_TRACK|drydock feature: DO_NOT_TRACK is set|\"containerEnv\":{\"DO_NOT_TRACK\":\"1\"}"
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC|drydock feature: CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC is set|\"containerEnv\":{\"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC\":\"1\"}"
	"DISABLE_GROWTHBOOK|drydock feature: DISABLE_GROWTHBOOK is set|\"containerEnv\":{\"DISABLE_GROWTHBOOK\":\"1\"}"
	# The same variable through remoteEnv, which lifecycle commands get too.
	"remoteEnv DO_NOT_TRACK|drydock feature: DO_NOT_TRACK is set|\"remoteEnv\":{\"DO_NOT_TRACK\":\"1\"}"
	# The shared volume missing: this container would sign in on its own.
	"no shared volume|drydock feature: nothing is mounted at /home/vscode/.claude|\"mounts\":[]"
	# The VS Code default-features shape (design §11, "Relationship to the
	# existing setup"): a per-container volume of its own, and
	# CLAUDE_CONFIG_DIR pointed at it. Drydock's mount is there too; nothing
	# fails without the check, and the login is simply not shared.
	"competing CLAUDE_CONFIG_DIR|drydock feature: CLAUDE_CONFIG_DIR is '/home/vscode/.claude-vscode'|\"mounts\":[$(mount "$vol2" /home/vscode/.claude-vscode)],\"containerEnv\":{\"CLAUDE_CONFIG_DIR\":\"/home/vscode/.claude-vscode\"}"
	# A second volume inside the shared one splits what is shared.
	"competing mount inside|drydock feature: /home/vscode/.claude/projects is a separate mount inside /home/vscode/.claude|\"mounts\":[$(mount "$vol2" /home/vscode/.claude/projects)]"
	# A second volume at exactly the shared volume's path: Docker refuses the
	# container before the Feature runs (above). Kept as a case so that if the
	# CLI ever merged the two, the run would come up and this would fail.
	"competing mount at the same path|Duplicate mount point: /home/vscode/.claude;;filesystems are mounted at /home/vscode/.claude|\"mounts\":[$(mount "$vol2" /home/vscode/.claude)]"
	"root remote user|drydock feature: the dev container's remote user is root|\"remoteUser\":\"root\""
)
vars="ANTHROPIC_BASE_URL DISABLE_TELEMETRY DO_NOT_TRACK CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC DISABLE_GROWTHBOOK"

pass=0 fail=0
for c in "${cases[@]}"; do
	IFS='|' read -r name want extra <<<"$c"
	[ -z "$filter" ] || [[ $name == *"$filter"* ]] || [ "$name" = baseline ] || continue
	ws=$work/$(echo "$name" | tr -c 'A-Za-z0-9\n' -)
	mkdir -p "$ws/.devcontainer"
	cp -r "$src" "$ws/.devcontainer/drydock"
	printf '{"image":"%s","features":{"./drydock":{"requireBroker":false}}%s}\n' "$image" "${extra:+,$extra}" >"$ws/.devcontainer/devcontainer.json"
	mount_flag=(--mount "type=volume,source=$vol,target=/home/vscode/.claude")
	[ "$name" != "no shared volume" ] || mount_flag=()
	set +e
	out=$(devcontainer up --workspace-folder "$ws" --id-label "$label" --id-label "drydock-expect-fail.case=$name" \
		"${mount_flag[@]}" 2>"$ws/log")
	code=$?
	set -e
	outcome=$(printf '%s' "$out" | jq -r '.outcome // empty' 2>/dev/null || true)
	why=""
	if [ "$want" = ok ]; then
		[ "$code" = 0 ] && [ "$outcome" = success ] || why="expected the build to succeed: exit $code, outcome '$outcome'"
	else
		if [ "$code" = 0 ] || [ "$outcome" != error ]; then
			why="expected the build to fail: exit $code, outcome '$outcome'"
		elif ! printf '%s\n' "${want//;;/$'\n'}" | grep -qF -f - "$ws/log"; then
			why="the build failed, but its log does not say: ${want//;;/ or }"
		else
			# A variable case names that variable and no other.
			for v in $vars; do
				[ "$v" = "$name" ] || [ "remoteEnv $v" = "$name" ] && continue
				if grep -q "drydock feature: $v is set" "$ws/log"; then why="it also named $v"; fi
			done
		fi
	fi
	if [ -z "$why" ]; then
		echo "PASS  $name"
		pass=$((pass + 1))
	else
		echo "FAIL  $name: $why"
		{ grep -E 'drydock feature|Error|error' "$ws/log" || true; } | tail -15 | sed 's/^/      /'
		fail=$((fail + 1))
	fi
	ids=$(docker ps -aq --filter "label=$label")
	# shellcheck disable=SC2086
	[ -z "$ids" ] || docker rm -f $ids >/dev/null
done

echo "$pass passed, $fail failed"
[ "$fail" = 0 ] && [ "$pass" -gt 1 ]
