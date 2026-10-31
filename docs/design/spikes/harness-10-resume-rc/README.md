# Spike 10 harness — hand a phone conversation to a laptop terminal

Answers: *can a terminal in the workspace's container take over a conversation started from the
Claude app against the `claude remote-control` server, with its history, and keep it usable in the
app?* Report: [`../10-resume-rc-handoff.md`](../10-resume-rc-handoff.md).

Built on [Spike 06's harness](../harness-06-attach/): `setup.sh` is Spike 06's, `lib.sh` sources
Spike 06's `lib.sh` and changes only names (the private tmux server is `dd-spike-10`, the server's
sessions are `ddspike10-…`), and transcripts are read with Spike 06's `tree.mjs`. Needs `claude` on
`PATH` (override with `CLAUDE_BIN=`), `tmux`, `jq`, `git`, `node`. Every live mode starts real Remote
Control sessions on the signed-in account and sends at most ten one-word turns. No container is
started: every process runs on the host under `env -i`, as in Spike 06.

```sh
cd docs/design/spikes/harness-10-resume-rc
./setup.sh /tmp/s10a && export S=/tmp/s10a   # a fresh scratch per handoff/picker run
./run.sh cli        # result 1: the flags, and the bridge's history-upload strings (no session started)
./run.sh handoff    # results 2-6: (a) --resume <id> --rc, (b) + --fork-session, (c) --continue --rc,
                    #   (d) --resume then /remote-control, (e) after the server's SIGTERM, (e2) the
                    #   server restarted beside it, (f) a second --rc while (e) holds it
./setup.sh /tmp/s10b && S=/tmp/s10b ./run.sh picker   # result 7: --resume --fork-session --rc, no id
./run.sh ids        # result 8: from Drydock's session_… id to the local id --resume takes
./run.sh worktree   # result 9: a `claude -w` worktree's conversation resumed from elsewhere
S=/tmp/s10a ./reap.sh   # only after a run was SIGKILLed: stop what it left on dd-spike-10
rm -rf /tmp/s10a /tmp/s10b   # they hold a live refresh token
```

`handoff` and `picker` refuse a scratch whose server session already has turns: the server's local
session id is stable across restarts on one config, so a second run would measure a used
conversation (the first draft of this harness did, once). `ids`, `worktree` and `cli` can share a
used scratch.

Each mode writes its logs to a fresh `$S/run/<mode>/` (tmux `pipe-pane` captures and `--debug-file`
logs) and prints the lines its result rests on.

## The human steps (H1–H5)

Only the app can send a turn into a server session or say where a session is listed, so the
unattended `handoff` seeds the history through a terminal that `--continue` opened on the server's
pre-created session (a stand-in, and labelled one in the report). The real thing:

```sh
./setup.sh /tmp/s10h && S=/tmp/s10h WAIT_APP=300 ./run.sh handoff
```

The run prints each step and waits up to `WAIT_APP` seconds for it:

- **H1.** It prints the environment link and the session's name. In the app, open that session and
  send *Remember the word QUOKKA… Reply with exactly APP1 and nothing else.* (the run prints the
  exact marker). The run continues when that turn lands in the server session's transcript. Note:
  does the app session now show the turn? (It should; it is the server's.)
- **H2.** After `(a) claude --resume <id> --rc` starts, it prints the terminal's app session URL.
  Open it. **Record:** is it listed in the server's environment or under **"Remote Control"**; does
  it show the earlier `APP1` turn and the marker (the bridge said `Flushing N history events`)?
  Then send *Reply with exactly APP2 and nothing else.* from the app; the run waits for it in the
  terminal, then types `TERM2` there. **Record:** does `TERM2` show in that app session; does it show
  in the server's original session (expected: no, Spike 06 H1)?
- **H3.** Go back to the server's original session in the environment and send *Reply with exactly
  APP3 and nothing else.* The run prints the shared transcript as a tree (two leaves = the file
  forked: two writers on one conversation) and whether `APP3` reached the terminal (expected: no).
- **H4.** Same as H2 for `(b) --resume <id> --fork-session --rc` (`APP4`, `TERM4`). **Record** the
  same three things. This is the variant the report recommends.
- **H5 — a `--spawn worktree` session (manual).** The harness cannot create one. In the app, start a
  *new* session in the same environment while the run is at H4 (the server spawns it in a worktree
  of the clone) and send *Remember the word WOMBAT. Reply with exactly OK5.* Then, in another shell
  on the host:

  ```sh
  export S=/tmp/s10h; CFG=$S/cfg
  # the new session's local id, from the server's child's registry row (newest first)
  for f in $CFG/sessions/*.json; do jq -c '{pid,sessionId,cwd,bridgeSessionId,entrypoint}' "$f"; done
  cd $S/ws && env -i HOME=$S/home PATH="$PATH" TERM=xterm-256color CLAUDE_CONFIG_DIR=$CFG \
    DISABLE_AUTOUPDATER=1 claude --resume <that sessionId> --fork-session --rc
  ```

  **Record:** where the server put the worktree (`cwd` in the row), whether the resume from the
  clone's path is accepted and which directory it runs in (the status line shows it), whether it
  recalls `WOMBAT`, and where the app lists its new session. Quit it with `/exit`.

Then let the run finish (it reaps what it started) and `rm -rf /tmp/s10h`.

## The files

- `setup.sh <scratch> [source-config]` runs Spike 06's `setup.sh`: `<scratch>/ws` (a git repo with
  one commit) and `<scratch>/cfg` seeded like the shared volume (credential, `oauthAccount`,
  `remoteDialogSeen`, trust for `ws`, plus `hasCompletedOnboarding`, which the Feature does not
  write). The source config is only read.
- `lib.sh`, sourced: Spike 06's helpers, renamed, plus `variant` (start a hand-off terminal and
  report it), `ask` (the history test, read from the transcript), `bridge` (the bridge's own debug
  lines) and `gained`.
- `run.sh <mode>`, the modes above; `reap.sh`, cleanup after a killed run.

## Traps worth knowing if you edit this

All of Spike 06's (its README), and:

- **The token guard.** The scratch config holds a copy of the owner's live refresh token. `setup.sh`
  and every mode refuse unless the access token's `expiresAt` is at least `TOKEN_MARGIN_MIN`
  (default 60, floor 30, whole numbers only; `is_uint` before any arithmetic, every check
  `[ ok ] || refuse`) minutes away, so no run reaches Claude Code's refresh window. Delete the scratch
  when done.
- **The screen is not evidence of an answer.** A resumed session redraws its history, so the last
  `●` line on screen can be an earlier turn's answer while the question went nowhere. In the first
  run `/remote-control` on a session that already had Remote Control opened a panel (*Disconnect
  this session / Show QR code / Continue*) that swallowed the next line typed. `ask` reads the answer
  from the transcript instead, only a record written after the question, and `(d)` sends `Escape`
  after the panel.
- **A conversation remembers its app session.** Once a terminal held Remote Control on it, the
  transcript carries `bridge-session` records, and any later `--resume`/`--continue` of it —
  even without `--rc` — reattaches that app session. So the variants are not independent: `(c)`,
  `(d)`, `(e)` and `(f)` reattach what `(a)` or `(b)` minted, and the report reads them that way.
- **A resumed conversation writes the file it started in**, not `projects/<cwd>/`: `ask` finds the
  newest `projects/*/<id>.jsonl`.
- **`claude -w` locks its worktree** with its pid (`git worktree list` says `locked`); the cleanup
  uses `git worktree remove -f -f`, on this harness's own scratch only.
- **Signals.** `run.sh` traps `TERM`/`INT`/`HUP` so its `EXIT` trap reaps after a `timeout`; a run
  killed with `SIGKILL` leaves its panes, and `reap.sh` stops them (the server pane last, `SIGTERM`
  first). Nothing outside the private `dd-spike-10` tmux server is ever signalled.
