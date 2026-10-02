#!/usr/bin/env bash
# The script CLAUDE_ENV_FILE points at.
#
# Claude Code reads this file as *text* and composes it into a shell prelude for
# Bash tool commands, so every line here runs in the same shell as the command
# itself. That is what makes the counter below a measurement: it increments once
# per actual execution, so a Bash command that sees a higher value than the
# previous one proves the prelude ran again rather than being captured once.
#
# DRYDOCK_SPIKE_* come from the driver's environment, which the prelude inherits
# via the claude process. run.sh copies this file into the run directory and
# points CLAUDE_ENV_FILE at the copy, so the `mutate` mode below can rewrite it
# without touching the checked-in original.
STATE="${DRYDOCK_SPIKE_STATE:-/tmp/drydock-spike}"
MODE="${DRYDOCK_SPIKE_MODE:-basic}"
SELF="${DRYDOCK_SPIKE_ENVFILE:-}"

n=$(( $(cat "$STATE/counter" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$STATE/counter"
printf 'exec n=%s pid=%s ppid=%s cwd=%s\n' "$n" "$$" "$PPID" "$PWD" >> "$STATE/execlog"

# Does the composed prelude reach the shell as an argv string? If it does, the
# text of this file is visible in `ps` — which is fine for a one-liner that
# delegates to a helper, and a leak if secret values were ever inlined instead.
tr '\0' ' ' < "/proc/$$/cmdline" 2>/dev/null \
  | cut -c1-240 | sed "s/^/argv n=$n: /" >> "$STATE/argvlog"
printf '\n' >> "$STATE/argvlog"

# A prelude that dies before it can export anything, standing in for a broker
# socket that is not there. Deliberately placed ahead of the export so the test
# distinguishes "command aborted" from "command ran with the variable unset".
if [ "$MODE" = "failing-early" ]; then
  echo "PRELUDE-FAILURE: broker unreachable" >&2
  exit 7
fi

# Stand-in for `eval "$(drydock-secrets export)"`: a value that only the current
# execution could know.
export DRYDOCK_SPIKE_VALUE="v$n"

case "$MODE" in
  noisy)
    # Does prelude output pollute what the agent sees as command output?
    echo "PRELUDE-STDOUT-LEAK"
    echo "PRELUDE-STDERR-LEAK" >&2
    ;;
  failing)
    # Broker down, but only after the export. Does the command still run?
    echo "PRELUDE-FAILURE: broker unreachable" >&2
    false
    ;;
  mutate)
    # Rewrite the env file itself on the first execution. If a later command
    # sees DRYDOCK_SPIKE_MUTATED set, Claude Code re-reads the file from disk
    # per command; if it never appears, the script text is cached for the
    # session even though it is re-executed.
    if [ "$n" = 1 ] && [ -n "$SELF" ]; then
      printf '\nexport DRYDOCK_SPIKE_MUTATED=yes\n' >> "$SELF"
    fi
    ;;
esac
