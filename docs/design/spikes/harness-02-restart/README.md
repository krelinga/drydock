# Spike 02 harness — `remote-control` restart survival

Answers: *does re-running `claude remote-control` in the same directory bring back the sessions the
previous server was serving?* Report: [`../02-rc-restart.md`](../02-rc-restart.md).

```sh
mkdir -p /tmp/ws && git -C /tmp/ws init -q && git -C /tmp/ws commit -qm init --allow-empty
./seed-config.sh /tmp/cfg /tmp/ws      # pass all three headless startup gates
export CLAUDE_CONFIG_DIR=/tmp/cfg

./run.sh sigterm   /tmp/ws   # graceful stop   -> environment + session preserved
./run.sh sigkill   /tmp/ws   # hard kill       -> environment + session preserved
./run.sh nosession /tmp/ws   # kill with no live session -> 409 for 1-3 minutes
./run.sh both      /tmp/ws   # sigterm then sigkill
```

`seed-config.sh <config-dir> <workspace-dir> [source-config-dir]` writes a `CLAUDE_CONFIG_DIR` that
starts Remote Control with no TTY interaction: the credential, the `oauthAccount` record (**not** in
`.credentials.json`, and Remote Control refuses without it), `remoteDialogSeen`, and the workspace
trust record. It reads your real `~/.claude` by default and never modifies it.

`run.sh [mode] [dir]` starts a server under tmux, waits until it reports a live session (`Capacity:
1/4` — the distinction between *listening* and *serving* is what the two outcomes hinge on), signals
it, then restarts and compares the environment and session ids. Each round settles for `SETTLE`
seconds (default 120) first, so a registration left by a previous round is not mistaken for this
one's; `MAX_WAIT` (default 300) bounds the restart polling. Every round reaps the servers it started.

Two traps worth knowing if you edit this:

- **tmux panes inherit the tmux *server's* environment**, not the invoking shell's, so
  `CLAUDE_CONFIG_DIR` is passed explicitly via `env` in the pane command. Without it the server
  silently reads `~/.claude` and reports the workspace untrusted.
- **`pkill -f` matches your own command line.** The reap pattern is written `worktre[e]` so it cannot
  match the pattern text itself, and the signalling path uses the tmux pane pid (the server is
  `exec`'d, so the pane pid *is* the server) rather than a `ps | grep`.

Runs start real Remote Control servers on the signed-in account.
