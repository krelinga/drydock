# Spike 08 harness — `claude --bg --remote-control` per workspace container

Answers phase 2 of [`../08-bg-rc.md`](../08-bg-rc.md): *is one background Remote Control session per
workspace container, run by Claude Code's own daemon, operationally workable for Drydock?* Isolation
between workspaces is out of scope (design §13, the one-trust-domain model).

Needs `docker` (the devcontainer's docker-in-docker is enough), `tmux`, `jq`, and the pinned
`claude` on the host's `PATH` (override with `CLAUDE_BIN=`). The binary is bind-mounted read-only into
each scratch container, so the version under test is the one every mode prints first (`2.1.289` for
the recorded results). Every mode starts real Remote Control sessions on the signed-in account; only
`env` (and the human `app` mode) sends turns.

```sh
cd docs/design/spikes/harness-08-bg-rc
./setup.sh /tmp/s08            # ws-a, ws-b, one shared cfg, the dd-spike-08 image
export S=/tmp/s08

./run.sh cli                   # result 1: --bg, agents, attach, logs, stop, respawn, rm, daemon
./run.sh start                 # results 2, 3: a headless start over docker exec; ids, link, files; outlives the exec
./run.sh gates                 # result 4: trust, remoteDialogSeen, onboarding, oauthAccount removed one at a time
./run.sh env                   # result 5: the session's environment, and CLAUDE_ENV_FILE (two turns; NO_TURN=1 skips them)
./run.sh stop                  # result 6: claude stop, SIGTERM/SIGKILL of the worker, SIGTERM of the daemon, respawn
./run.sh daemon                # result 7: `claude daemon run` held in the foreground; `daemon stop --any`; SIGTERM
./run.sh two                   # result 8: two containers on one config, both at /workspace
DISTINCT=1 ./run.sh two        # result 8: the same at /workspaces/ws-a and /workspaces/ws-b
./run.sh lifecycle             # result 9: docker stop/start and rm/run with a job live
./run.sh refresh               # result 10: reading only, the refresh path (runs no claude)

# the human steps (below)
WAIT_APP=300 ./run.sh app      # H1, H2
```

Each mode writes to a fresh `$S/run/<mode>/` and prints the lines its result rests on. When done:
`rm -rf "$S"` (it holds a live token) and `docker rmi dd-spike-08`.

## The files

- `Dockerfile` — the stand-in workspace image: Debian 12, `git`, `jq`, `procps`, `tmux`, a `vscode`
  user at uid 1000. Not the Feature.
- `setup.sh <scratch> [source-config]` — the token guard, two one-commit repos, and one config seeded
  by Spike 02's [`seed-config.sh`](../harness-02-restart/seed-config.sh) (credential, `oauthAccount`,
  `remoteDialogSeen`), with trust for `/workspace`, `/workspaces/ws-a` and `/workspaces/ws-b` and
  Spike 06's onboarding seed. It also writes a stand-in `CLAUDE_ENV_FILE` that appends a marker line
  to `<workspace>/.env-file-ran`. The source config is only read.
- `lib.sh`, sourced — the containers, exec and scrape helpers, `whoruns`, and `reap`.
- `run.sh <mode>` — the modes above.

## How a container is run

Like a Drydock workspace, as far as this question reaches: `docker run` with PID 1 the devcontainer
CLI's own keep-alive loop (`sh -c '… trap "exit 0" 15; while sleep 1 & wait $!; do :; done'`, which
exits at once on `SIGTERM`), the shared config at the Feature's `CLAUDE_CONFIG_DIR`
(`/home/vscode/.claude`), the Feature's `DISABLE_AUTOUPDATER=1` and `CLAUDE_ENV_FILE` as container
environment, and every command a `docker exec -u vscode` (what `devcontainer exec` is). Not here: the
Feature's install and `postCreate`, the broker socket, `devcontainer up` itself.

## Traps worth knowing if you edit this

- **The token guard** is Spike 06's, unchanged: the scratch config holds a copy of the owner's live
  refresh token; a refresh of the copy rotates the real one dead. Every `--bg` daemon schedules a
  proactive refresh **four minutes before `expiresAt`** (result 10), so every mode refuses unless
  `expiresAt` is at least `TOKEN_MARGIN_MIN` (60) minutes away, and `reap` removes every container —
  a daemon left running would refresh the copy on its own schedule.
- **Cleanup is by recorded name.** `ctr_up` records each container in `$S/ctr.names`, `bg` each job
  in `$S/bg.ids`; `reap` stops those jobs, `docker stop`s (SIGTERM, then SIGKILL after 10 s) and
  removes those containers, and kills the private tmux server `-L dd-spike-08`. Nothing outside the
  scratch containers is signalled.
- **Each mode starts from empty daemon state** (`fresh_state` deletes `daemon/`, `jobs/`,
  `sessions/` and `daemon.*` in the *scratch* config): jobs outlive their containers in the shared
  directory, and a listing would otherwise carry every earlier mode's.
- **`/proc/<pid>/environ` of a uid-1000 process is not readable by the container's root** (no
  `CAP_SYS_PTRACE`), and reads as empty rather than failing loudly through a pipe; `env_of` reads it
  as `vscode`.
- **A pid names nothing across containers.** Each container has its own pid namespace and the
  shared `sessions/<pid>.json` rows collide by pid. A row carries `pidDomain`
  (`linux::pid:[<inode>]`); `whoruns` matches it to the container whose `/proc/1/ns/pid` it is, and
  calls the row alive only if `/proc/<pid>/stat`'s start time equals the row's `procStart` — a new
  container can reuse a removed one's namespace inode (seen in `lifecycle`).
- **Claude Code's processes retitle themselves** (`claude daemon run …`, `claude bg-spare …`): a
  worker that claimed a pre-spawned spare keeps the spare's argv, so `ps` alone does not name its job.
  `wpid` takes the pid from `claude agents --json`, and `whoruns` reads the registry.
- **`daemon.log` is shared** by every daemon on the config, and its lines carry no pid; `dlog_since`
  prints it by time, so in `two` read it beside the per-container process lists.
- **`--debug-file` is carried into the worker** (and kept by `respawn`), which is how `stop`,
  `daemon`, `lifecycle` and `app` see the bridge's teardown (`Archive … status=200`) or its absence.

## The human steps

The harness cannot see the app. These are the checks owed to the owner, numbered as in the report.

- **H1 — the session in the app.** `WAIT_APP=300 ./run.sh app`. When it prints a
  `claude.ai/code/session_…` link, open it (or find `ddspike08app` under the app's **Remote
  Control** section) and send `Reply with exactly APP1 and nothing else.` Record: where the app lists
  it, its title, and whether `APP1` comes back.
- **H2 — stop and respawn as the app sees them.** Same run. After `claude stop` (the harness prints
  `archive=200`), record what the app shows for the session (archived, offline, gone) — **H2a**. After
  `claude respawn` (the harness prints `Unarchive … status=200` and the same session id), record
  whether it is back with `APP1` in its history, and send `Reply with exactly APP2.` — **H2b**.
- **H3 — the daemon's refresh against Spike 00's lock, live.** Not runnable on a copied credential,
  ever: a refresh rotates the refresh token, and the owner's real login is then blanked. It needs a
  **login of its own**, which only the owner can make:
  1. `docker volume create dd-s08-login` and, in a throwaway container mounting it at
     `/home/vscode/.claude` with the pinned `claude`, run `claude auth login` and complete it in a
     browser (design §7.2's handshake, by hand). Seed `remoteDialogSeen`, `hasCompletedOnboarding`
     and trust for `/workspace` in that volume's `.claude.json` (Spike 02's `seed-config.sh` does the
     first and third). This login is disposable: it is not the one Drydock or the owner uses.
  2. Two containers like this harness's (`ctr_up a <volume path>` / `b`), both on that volume; start
     a `--bg --remote-control` job in each, and in a third a plain `claude remote-control` server.
  3. To avoid waiting out the ~8 h access token, edit **that volume's** `expiresAt` to about six
     minutes from now (only this disposable login's file — never a copy of the owner's), so every
     daemon schedules its refresh within two minutes.
  4. Record from the shared `daemon.log`: one `auth: proactive refresh succeeded` and the other
     daemon's `auth: token still valid (cross-process refresh or not yet due)`; the
     `.oauth_refresh.lock` directory and its `.oauth_refresh.lock.owner` record (pid, `pidDomain`)
     while it is held; that `.credentials.json` holds a new `refreshToken` and **not** `accessToken:
     ""` (blanked); that the server and both sessions keep Remote Control (`bridgeSessionId` in
     `sessions/`). Then `claude daemon stop --any` in both, stop the server with `SIGTERM`, remove
     the containers and `docker volume rm dd-s08-login`, and sign that login out at claude.ai.
