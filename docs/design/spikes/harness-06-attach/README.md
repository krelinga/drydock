# Spike 06 harness — getting a terminal onto the session the app sees

Answers: *after attaching to a workspace's dev container in VS Code, can the operator also get onto
the Claude session the Claude app is driving?* Report: [`../06-terminal-attach.md`](../06-terminal-attach.md).

Needs `claude` on `PATH` (override with `CLAUDE_BIN=`), `tmux`, `jq`, `git` and `node` (for
`tree.mjs`, builtins only). Every mode starts real Remote Control sessions on the signed-in account
and sends at most three one-word turns.

```sh
cd docs/design/spikes/harness-06-attach
./setup.sh /tmp/s06            # /tmp/s06/ws (a git repo) and /tmp/s06/cfg (a seeded CLAUDE_CONFIG_DIR)
export S=/tmp/s06

./run.sh cli                   # results 1, 3, 9: --help, strings, the server session's argv, dead-end probes
./run.sh server-resume         # result 2: --resume/--continue/picker/--teleport vs the server's session
./run.sh interactive-resume    # result 4: two terminals on one conversation; the takeover
./run.sh alongside             # result 5: interactive Remote Control beside the server; a worktree
./run.sh tmux-rc               # results 6, 7: claude --remote-control in tmux: ids, turns, resize, SIGTERM/SIGKILL
./run.sh tmux-rc-gates         # result 8: trust, remoteDialogSeen, oauthAccount, onboarding removed one at a time
./run.sh bg                    # result 10: claude --bg --remote-control and claude attach

# the human steps (see the report's "Owed" section)
WAIT_APP=300 ./run.sh server-resume   # send one turn from the app first; then the same probes
WAIT_APP=300 ./run.sh tmux-rc-app     # send one turn from the app to an interactive session
```

Each mode writes its logs to a fresh `$S/run/<mode>/` (tmux `pipe-pane` captures and `--debug-file`
logs) and prints the lines its result rests on.

## The files

- `setup.sh <scratch> [source-config]` builds the workspace and runs Spike 02's
  [`seed-config.sh`](../harness-02-restart/seed-config.sh), which copies the credential and passes the
  server's three startup gates (`oauthAccount`, `remoteDialogSeen`, trust). It then sets `hasCompletedOnboarding` and `theme`,
  which the Feature does **not** write: without them an interactive `claude` opens on the first-run
  theme picker (result 8). `NO_ONBOARDING_SEED=1` skips that. The source config is only read.
- `lib.sh`, sourced: the pane, wait, scrape and registry helpers, and `reap`.
- `run.sh <mode>`, the modes above.
- `tree.mjs <transcript.jsonl>` reads a transcript as the tree its `parentUuid` links make and prints
  each conversational leaf: two leaves sharing a prefix is two writers forking one file.

## Traps worth knowing if you edit this

- **The scratch config holds a copy of the owner's live refresh token, and a copy can kill the
  original.** The refresh lock is per `CLAUDE_CONFIG_DIR`, so a scratch copy refreshes on its own;
  a refresh rotates the refresh token (Spike 00), and the real config's next refresh then finds its
  token dead and *blanks* the login. Claude Code refreshes only near the access token's `expiresAt`
  (the `--bg` daemon schedules it a few minutes before), so `setup.sh` and every `run.sh` mode
  refuse to start unless `expiresAt` is at least `TOKEN_MARGIN_MIN` (default 60; at least 30, whole numbers only) minutes away. If
  they refuse, let the real config refresh (use `claude` normally), then re-run `setup.sh`. **Delete
  the scratch directory when you are done** (`rm -rf "$S"`): it holds a live token.
- **Signal only what the harness started.** `bg` records each background-session id it is handed in
  `$S/bg.ids`, and `reap` stops those ids and nothing else — never every session `claude agents`
  lists, since the daemon's sockets are keyed by uid and the list may hold the owner's. The
  daemon's pid is found as the worker's grandparent and is signalled only after its command line
  (`daemon run`) and its `CLAUDE_CONFIG_DIR` are checked.

- **Everything runs under `env -i`** with only what a Drydock container gives Claude Code: a scratch
  `HOME`, `PATH`, `TERM`, `LANG`, `CLAUDE_CONFIG_DIR` and `DISABLE_AUTOUPDATER=1`. tmux panes inherit
  the tmux *server's* environment (Spike 02's trap), and a harness run from inside a Claude Code
  session would otherwise hand every child `CLAUDECODE=1`, `CLAUDE_CODE_SESSION_ID` and friends.
- **`HOME` is a scratch one too.** `remote-control` writes each spawned session's system-prompt file
  under `$HOME/.claude/bridge-spawn/`, not under `CLAUDE_CONFIG_DIR`, so a real `HOME` is written to
  even with a throwaway config.
- **A private tmux server** (`tmux -L dd-spike-06`): `reap` kills that server and nothing else, so an
  operator's own tmux sessions — a Claude Code session among them — are never touched. Processes are
  signalled by tmux pane pid (each command is `exec`'d), never by `pkill -f`.
- **A slash command needs a trailing space before Enter.** `/remote-control` + Enter picks the
  highlighted autocomplete entry, which was `/remote-env`; `typeline` sends `'/remote-control '`.
- **A `SIGKILL`ed process leaves its `$CFG/sessions/<pid>.json` row**, so `registry` marks dead pids
  and `holders`/`uuid_of` read live rows only. The server's registry row can land a few seconds after
  `Capacity: 1/4`; `uuid_of` waits for it.
- **One-shot commands get stdin closed** (`cenv`): `claude remote-control --help` under `env -i` with
  an open stdin did not return.
- `--bg` sessions live in Claude Code's own daemon (`$CFG/daemon/`, sockets under
  `/tmp/cc-daemon-<uid>/`), which exits by itself five seconds after its last session ends.
- **"The process logged nothing" needs a live log.** `dbg_gained` refuses a missing debug file loudly
  and prints how many lines the file gained (its heartbeats show it is still being written) and
  every line that is not a heartbeat.
