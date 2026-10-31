# Spike 09 harness — the split credential layout

Answers [`../09-split-credential.md`](../09-split-credential.md): *can each workspace container have its
own `CLAUDE_CONFIG_DIR`, share only the credential through `CLAUDE_SECURESTORAGE_CONFIG_DIR`, and run
one `claude --bg --rc` conversation the phone and the laptop both use?*

Needs `docker` (the devcontainer's docker-in-docker is enough), `tmux`, `jq`, and the pinned `claude`
on the host's `PATH` (override with `CLAUDE_BIN=`), bind-mounted read-only into each scratch
container, so the version under test is the one every mode prints first (`2.1.289` for the recorded
results).

```sh
cd docs/design/spikes/harness-09-split-credential
./setup.sh /tmp/s09            # ws-a..c, the shared cred dir (a copy), oauthAccount.json, base.claude.json, the image
export S=/tmp/s09

# fake credential only — no real token is ever mounted
./run.sh read                  # result 6 (read): the variable's resolver and every use the results rest on
./run.sh bump                  # result 7: the bump-ritual check; exits 1 on any failure
./run.sh lock                  # results 1, 6: four containers, three kinds of refresher, one lock path
NOVAR=1 ./run.sh lock          # results 1, 6, the control: no variable, a lock per config dir
./run.sh blank                 # result 1: the tombstone lands in the shared dir; every container sees it
HOLD=1 ./run.sh blank          # result 1: with <dir>/.storage-write.lock held, the blanking write cannot land

# the owner's copied credential (token guard)
./run.sh seed                  # result 2: seeded / unseeded (the control) / env-seeded `--bg --rc`
./run.sh server                # result 2: today's `remote-control` server, seeded / unseeded
./run.sh two                   # result 3: two containers, own daemons, both at /workspace
DISTINCT=1 ./run.sh two        # result 3: the same at /workspaces/ws-a and /workspaces/ws-b
./run.sh attach                # result 4: `claude attach` from a second exec, one turn
./run.sh supervise             # result 5: Spike 08's supervision recap under split

# the human steps (below)
WAIT_APP=300 ./run.sh app      # H1
./h2.sh up|login|expire|start|record|down   # H2, on a disposable login
```

Each mode writes to a fresh `$S/run/<mode>/` and prints the lines its result rests on. `DISTINCT=1`
is for `two` only, `NOVAR=1` for `lock`, `HOLD=1` for `blank`; every other mode refuses them. When
done: `./sweep.sh`, then `rm -rf "$S"` (it holds a live token) and `docker rmi dd-spike-09`.

**Runs must not overlap.** Each `run.sh`/`setup.sh` starts with `sweep.sh`, which removes every
container labelled `dd-spike-09=1`. **If a run was interrupted**, run `./sweep.sh` before anything
else and before `rm -rf "$S"`: a daemon in a leftover container holds the copied refresh token in
memory and refreshes it on its own schedule.

## The layout under test

| | in the container | on the host (`run.sh`) | in `h2.sh` |
|---|---|---|---|
| each container's own config | `/home/vscode/.claude` = `CLAUDE_CONFIG_DIR` | `$S/cfg-<name>` | volume `dd-s09-h2-<c>` |
| the one shared credential dir | `/home/vscode/.claude-credentials` = `CLAUDE_SECURESTORAGE_CONFIG_DIR` | `$S/cred` (real copy), `$S/fake/cred` (fake) | volume `dd-s09-h2-cred` |

Every per-container `.claude.json` starts from `base.claude.json` (the Feature's `remoteDialogSeen` and
trust, plus Spike 06's `hasCompletedOnboarding` and `theme`); a *seeded* one also gets the source
login's `oauthAccount` object, merged by [`seed-oauth.sh`](seed-oauth.sh) under Claude Code's own
`.claude.json.lock` (the Feature's `drydock-claude-config` discipline: `mkdir` lock, never stolen,
temp file + `rename(2)`). A container is otherwise run as Spike 08 runs one: PID 1 the devcontainer
CLI's sleep loop, `DISABLE_AUTOUPDATER=1` and `CLAUDE_ENV_FILE` in its environment, every command a
`docker exec -u vscode`.

## The files

- `Dockerfile` — the stand-in image: Debian 12, `git`, `jq`, `procps`, `tmux`, `vscode` at uid 1000,
  and both mount points made and owned by `vscode` (so an empty named volume takes that owner).
- `setup.sh <scratch> [source-config]` — the token guard, three one-commit repos, the shared cred dir
  (a copy of the source's `.credentials.json` and nothing else), `oauthAccount.json` (the source's
  record alone), `base.claude.json`, a stand-in `CLAUDE_ENV_FILE`. The source config is only read.
- `seed-oauth.sh <config-dir> <oauthAccount.json>` — the seed, under `.claude.json.lock`.
- `lib.sh`, sourced — containers, exec and scrape helpers, `watch_locks`, the fake credential, `reap`.
- `run.sh <mode>` — the modes above. `h2.sh` — human step H2.
- `sweep.sh` — removes every container carrying the label, whatever run left it.

## Traps worth knowing if you edit this

- **The token guard** (Spike 08's): `seed`, `server`, `two`, `attach`, `supervise` and `app` refuse
  unless the copy's `expiresAt` is `TOKEN_MARGIN_MIN` (60, at least 30) minutes away plus the mode's
  own minutes. It covers only the scheduled refresh; a `401` on the copy (revoked server-side) would
  refresh it at once — the residual Spikes 06 and 08 carry.
- **The fake modes never mount the real copy**: `lock`, `blank` and `bump` set the cred dir to
  `$S/fake/cred` and refuse to run if it resolves to `$S/cred`. Their credential is Spike 00's fake
  (`sk-ant-…-SPIKE09-FAKE-…`). `lock` and `bump` blackhole `platform.claude.com` (the binary's
  `TOKEN_URL`) to a TEST-NET-3 address, so a refresh's connect hangs and its lock is held long enough
  to see; `blank` does not, so the token endpoint refuses the fake refresh token and Claude Code blanks
  the fake. The fake access token also draws `401`s from the API — harmless, as in Spike 00.
- **`watch_locks` polls from the host** (20 ms; `WATCH_SLEEP=0` for as fast as bash can) and maps each
  `.oauth_refresh.lock.owner` record's `pidDomain` to the container whose pid namespace it is. A
  removed container's namespace inode can be reused by a new one (Spike 08's trap), so between steps
  `bump` clears a lock its own killed process abandoned — in the fake dir only, never a real one.
- **The credential's write lock is too brief to see by polling**, even flat out. `HOLD=1 blank`
  measures it the other way: it holds `<dir>/.storage-write.lock` (touched under its 15 s staleness)
  and shows the blanking write cannot land. The toucher is stopped at the end of the mode; it must
  only `touch -c`, or a stray one re-creates the lock as a file and blocks the next run's control.
- **The legacy refresh lock** is `<realpath(dir)>.lock`, *beside* the credential dir, so in the
  container's own filesystem (`/home/vscode/.claude-credentials.lock`; `/home/vscode/.claude.lock`
  without the variable). `lock` samples it from inside each container (`legacy_watch`).
- **`timeout` ends the `docker exec` client, not the process inside**, so a `claude -p` cut off by
  `XT` keeps running (and holding its lock) until the container goes.
- **A `remote-control` server on a fresh config asks for a spawn mode** (`Choose [1/2]`) unless given
  `--spawn`; `server` runs Drydock's own argv (`--spawn worktree --capacity 4 --verbose`).

## The human steps

- **H1 — the owner's use case, end to end.** `WAIT_APP=300 ./run.sh app`. When it prints a
  `claude.ai/code/session_…` link, open it on the phone (or find `ddspike09app` in the app) and send
  `Reply with exactly APP1 and nothing else.` — **H1a**: is `APP1` answered? Then, when the harness
  prints a `docker exec -it … claude attach <id>` command, run it in a terminal on this machine (the
  laptop's side), check `APP1` is in the history, and type `Reply with exactly TERM2 and nothing
  else.` — **H1b**: is `TERM2` answered in the terminal, and does the phone show it as a turn of the
  same session? Detach or close the terminal. The harness then reports what `claude logs` saw.
- **H2 — the refresh, live, on a disposable login.** Never runnable on a copied credential: a
  refresh rotates the refresh token and the copied login is then blanked. The owner makes a login
  for the run:
  1. `./h2.sh up` — the volumes and a login container (the shared credential volume at
     `/home/vscode/.claude-credentials` with the variable set; its own config volume).
  2. `./h2.sh login` — `claude auth login --claudeai` in it; complete it in a browser. It prints the
     credential's shape (lengths only) and whether the login's `.claude.json` has an organization.
  3. `./h2.sh expire` — sets **this disposable login's** `expiresAt` to now + 6 min, before any
     daemon starts (a daemon schedules its refresh when it starts; `expire` refuses while a, b or c
     runs).
  4. `./h2.sh start` — at once: containers a, b, c, each with its own config volume, seeded with the
     login's `oauthAccount` by `seed-oauth.sh`; `claude --bg --remote-control` in a and b, and
     Drydock's `claude remote-control --spawn worktree --capacity 4 --verbose` in c. Each daemon's
     `scheduling proactive refresh in …s` should name about two minutes.
  5. `./h2.sh record` — every 30 s for 10 min (`RECORD_MIN=`). Record: one `auth: proactive refresh
     succeeded` and the other daemon's `token still valid (cross-process refresh …)` — or, if the
     loser gives up as it did under contention in `lock` (`proactive refresh failed, signalling
     re-auth required` … `will re-check keychain every 30s`), whether it then recovers by itself;
     the `.oauth_refresh.lock` and its `.owner` (pid, `pidDomain`) while held; that the credential
     holds a new refresh token (`refreshTokenSum` changes) and is **not** blanked (`accessToken: 0`);
     and that a, b and c keep Remote Control (session ids in `sessions/`, the server's `env_…`).
  6. `./h2.sh down` — `claude daemon stop --any` in a and b, `SIGTERM` of the server, `claude auth
     logout`, the containers and volumes removed. Then sign that login out at claude.ai too.

  `H2_FAKE=1 ./h2.sh up` … `down` is the dry run (a fake credential instead of a login); it was run
  end to end before this was written, so the owner's run is not the script's first.
