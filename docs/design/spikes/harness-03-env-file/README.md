# Spike 03 harness — `CLAUDE_ENV_FILE`

Answers: *does Claude Code re-read and re-execute `CLAUDE_ENV_FILE` before each Bash command?*
Report: [`../03-claude-env-file.md`](../03-claude-env-file.md).

```sh
./run.sh basic          # 1:1 execution per Bash command (the headline result)
./run.sh mutate         # is the script text re-read from disk, or cached per session?
./run.sh noisy          # does prelude stdout/stderr reach the agent's tool results?
./run.sh failing        # non-zero last command: does the command still run?
./run.sh failing-early  # `exit 7` before the export: does the command run at all?
```

`envfile.sh` is the script under test. It bumps a counter and exports the new value, so a Bash
command that sees a different value than the previous one proves the script ran again — the
measurement does not depend on the agent cooperating or on timing. It also appends to `execlog`
(executions, ground truth) and `argvlog` (how the prelude reached the shell).

`run.sh` copies `envfile.sh` into a fresh run directory and points `CLAUDE_ENV_FILE` at the copy, so
`mutate` can rewrite it without dirtying the checked-in original. Each run prints the Bash calls the
agent made, what each one saw, the execution log, the `argv` capture, and Claude Code's own debug
lines about the session environment. Artifacts stay in the run directory, named at the end of the
output.

No real secret is involved: the exported value is a counter. Requires `claude` on `PATH` (override
with `CLAUDE_BIN=`) and `jq`. Runs `claude -p` with `--dangerously-skip-permissions` in an empty
scratch directory — safe here because the only commands in play are the three `echo`s in the prompt,
and necessary because a headless run cannot answer a permission prompt.
