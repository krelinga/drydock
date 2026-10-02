#!/usr/bin/env bash
# Spike 03 — is CLAUDE_ENV_FILE re-read (and re-executed) before each Bash command?
#
# Usage: ./run.sh [basic|mutate|noisy|failing|failing-early]
#
# Drives a headless `claude -p` session that issues three separate Bash calls,
# each echoing a variable the CLAUDE_ENV_FILE prelude sets from a counter it
# bumps itself. Three distinct values means the prelude runs per command, which
# is what design §10.3's "the environment pulls" route needs. Three identical
# values would mean the environment is captured once at session start.
#
# No real secret is involved; the exported value is a counter.
set -uo pipefail

MODE="${1:-basic}"
S="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLAUDE="${CLAUDE_BIN:-$(command -v claude)}"
RUN="${DRYDOCK_SPIKE_RUN:-$(mktemp -d -t drydock-spike-03-XXXXXX)}"

export DRYDOCK_SPIKE_STATE="$RUN/state"
export DRYDOCK_SPIKE_MODE="$MODE"
# Run against a copy, so `mutate` can rewrite the env file without dirtying the
# checked-in original.
export DRYDOCK_SPIKE_ENVFILE="$RUN/envfile.sh"
export CLAUDE_ENV_FILE="$DRYDOCK_SPIKE_ENVFILE"

mkdir -p "$DRYDOCK_SPIKE_STATE" "$RUN/cwd"
cp "$S/envfile.sh" "$DRYDOCK_SPIKE_ENVFILE"
: > "$DRYDOCK_SPIKE_STATE/execlog"
: > "$DRYDOCK_SPIKE_STATE/argvlog"
echo 0 > "$DRYDOCK_SPIKE_STATE/counter"

echo "claude under test: $("$CLAUDE" --version 2>&1 | head -1)"
echo "mode=$MODE  run=$RUN"
echo "CLAUDE_ENV_FILE=$CLAUDE_ENV_FILE"
echo

PROMPT='Run each of these as its own separate Bash tool call. Do not combine them into one call, and do not use && or ; to chain them.

1. echo "READ-A=$DRYDOCK_SPIKE_VALUE mutated=${DRYDOCK_SPIKE_MUTATED:-unset}"
2. echo "READ-B=$DRYDOCK_SPIKE_VALUE mutated=${DRYDOCK_SPIKE_MUTATED:-unset}"
3. echo "READ-C=$DRYDOCK_SPIKE_VALUE mutated=${DRYDOCK_SPIKE_MUTATED:-unset}"

Then reply with the three output lines verbatim, nothing else.'

cd "$RUN/cwd" || exit 1
"$CLAUDE" -p "$PROMPT" \
  --output-format stream-json --verbose \
  --dangerously-skip-permissions \
  --allowed-tools Bash \
  --max-turns 12 \
  --debug --debug-file "$RUN/debug.log" > "$RUN/stream.jsonl" 2> "$RUN/stderr.log"
rc=$?
cd "$S" || exit 1

echo "=== claude exit=$rc ==="
echo
echo "--- Bash commands the agent actually ran, and what each saw ---"
jq -r 'select(.type=="assistant") | .message.content[]? | select(.type=="tool_use" and .name=="Bash")
       | "  CMD  " + (.input.command|tostring)' "$RUN/stream.jsonl" 2>/dev/null
jq -r 'select(.type=="user") | .message.content[]? | select(.type=="tool_result")
       | "  OUT  " + ((.content[]?.text // .content // "")|tostring|gsub("\n";"\\n"))' \
       "$RUN/stream.jsonl" 2>/dev/null
echo
echo "--- prelude executions (ground truth, written by envfile.sh itself) ---"
cat "$DRYDOCK_SPIKE_STATE/execlog"
echo "  total=$(wc -l < "$DRYDOCK_SPIKE_STATE/execlog")  counter=$(cat "$DRYDOCK_SPIKE_STATE/counter")"
echo
echo "--- how the prelude reached the shell (argv of the command process) ---"
sed 's/^/  /' "$DRYDOCK_SPIKE_STATE/argvlog" | grep -v '^  $'
echo
echo "--- what Claude Code logged about the session environment ---"
grep -aiE 'session environment|CLAUDE_ENV_FILE' "$RUN/debug.log" | sed 's/^/  /' | sort -u
echo
echo "--- final assistant message ---"
jq -r 'select(.type=="result") | .result // empty' "$RUN/stream.jsonl" 2>/dev/null | sed 's/^/  /'
echo
echo "artifacts: $RUN"
