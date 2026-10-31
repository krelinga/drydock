# Spike 08 — Is `claude --bg --rc` safe and workable per workspace?

**Question.** [Spike 06](06-terminal-attach.md) found that Claude Code's own background mode,
`claude --bg --remote-control` with `claude attach <id>`, gives an operator a conversation shared
between a terminal and the Claude app with no tmux. It held that back until measured in two
containers, because the daemon keeps its state in `CLAUDE_CONFIG_DIR`, which in Drydock is the
volume every workspace shares (design §7.1), and refreshes the shared OAuth credential on its own
schedule. This spike asks the two halves of that: is it *safe* to run per workspace on a shared
volume, and is it *workable* (can Drydock supervise it, and does it behave on the volume)?

**Status: phase 1 done (a reading of the `2.1.289` binary, nothing measured); phase 2 done
(measured in scratch containers on one shared config), but for three human steps (H1–H3, owed).**

**Verdict, phase 2: not workable as Drydock's per-workspace model on the shared volume; keep the
server.** One container's daemon runs, refuses or corrupts another container's jobs through the shared
`jobs/` and roster: with the same workspace folder in two containers — which any two repositories
whose `devcontainer.json` pins the same `workspaceFolder` give, not only two that share a name — a
job launched in B **ran in A** (result 8); with distinct folders it ran in the right container, but
the other daemon marked A's live job dead, so `claude agents` called a serving session `failed` in
both containers, and a `claude stop` from
the wrong container printed `stopped` and stopped nothing. Inside one container it is supervisable
— a headless start, a file to read the link from, a stop that archives the session, a restart that
keeps it — but by different means than the server's, and with Spike 06's price on top (one
conversation per process, no environment, nothing for the app to start a new session in). Beside the
server, as an operator's own tool, it works **with conditions**: one workspace at a time using
`--bg`, a workspace folder no other running container shares, and nothing trusting `claude agents`.
Spike 06's cheaper answer, `claude --rc` in a terminal, needs no daemon and stays the recommendation.

**Answer to "safe", phase 1.** The question turned out to be bigger than `--bg`. Reading the binary
shows the shared volume already carries everything Claude Code treats as the *user's* configuration,
and much of that runs code. So code in one workspace container that can write to
`CLAUDE_CONFIG_DIR` can very likely run code in every other workspace's Claude Code — and that
Claude Code has its own workspace's broker socket in reach, so its repository's GitHub token and its
secrets. `--bg` adds a second path of the same kind: its roster, jobs and launch requests are in the
shared directory. This contradicts the security review's §1.2 *Compromised container* row as it was
written ("Wants: another repo's token, another workspace" — not reachable), with or without `--bg`.

**Decision (the owner, 2026-10-30).** Isolation between workspaces is not a goal. Drydock adopts the
threat model *repositories are trusted; workspaces are mutually trusted, one trust domain; the
agent's own actions are not trusted, but are held only at a few cheap chokepoints* (design §13,
*The threat model*). The cross-workspace reach is accepted and documented (design §7.1, §13.4,
§15.2; security review addendum), not closed. "Safe" is therefore answered by the decision, and
phase 2 measures only whether `--bg --rc` is workable.

- **Phase 1 read from:** the Claude Code `2.1.289` binary (its bundled JavaScript), by reading only.
  **No binary was run for phase 1**, and no phase-1 result is a measurement. Each is marked *read*.
- **Phase 2 verified against:** Claude Code `2.1.289` (the pinned version; the host's binary,
  bind-mounted read-only into each container), Linux, docker-in-docker, scratch Debian 12 containers
  run as `devcontainer up` runs one (PID 1 the CLI's `sleep` loop), with the Feature's
  `CLAUDE_CONFIG_DIR`, `DISABLE_AUTOUPDATER=1` and `CLAUDE_ENV_FILE`, every command a `docker exec`
  as uid 1000, two containers on one scratch config seeded like the shared volume. Not the Feature's
  install, the broker, or `devcontainer up` itself. Each phase-2 result is *measured* unless marked
  *read*, and names its harness mode.
- **Date:** 2026-10-30.
- **Harness:** [`harness-08-bg-rc/`](harness-08-bg-rc/) (phase 2) — re-runnable; each result names
  its mode. H1–H3 are written there as human steps. Its token guard, like Spike 06's, keeps a run
  clear of the daemon's *scheduled* refresh of the copied credential; it cannot stop a refresh a
  `401` would trigger (a token revoked server-side), the residual Spike 06 carries too.

---

## Results (phase 1, read)

| # | Reading | Result | How |
|---|---|---|---|
| 1 | User-level `settings.json` under `CLAUDE_CONFIG_DIR` | its `hooks` are loaded by every Claude Code process whose `CLAUDE_CONFIG_DIR` it is, and run commands before and after tool calls | *read* |
| 2 | Plugins and skills under `CLAUDE_CONFIG_DIR` | loaded as the user's own, in every process on that directory | *read* |
| 3 | The user-level `CLAUDE.md` | read into every session's context | *read* |
| 4 | `.claude.json`'s `mcpServers` | user-scope MCP servers, started by every process on that directory (a `stdio` server is a command) | *read* |
| 5 | `claude --bg`'s daemon state | the roster (`daemon/`), the jobs (`jobs/`) and its launch requests are under `CLAUDE_CONFIG_DIR`; its sockets are under `/tmp/cc-daemon-<uid>/` (Spike 06, measured), so per container | *read* |
| 6 | `CLAUDE_SECURESTORAGE_CONFIG_DIR` | moves `.credentials.json` **and** `.oauth_refresh.lock` together to another directory, leaving the rest of `CLAUDE_CONFIG_DIR` where it is; undocumented (requested in [anthropics/claude-code#79223](https://github.com/anthropics/claude-code/issues/79223)) | *read* |

### 1 — The shared volume is one trust domain

In Drydock every workspace container mounts `drydock-claude-config` at `CLAUDE_CONFIG_DIR`
(design §7.1). Results 1–4 put four code-running or context-writing inputs in that one directory, so
a write by one container is an input to every other's Claude Code. The likely path: a
prompt-injected agent in workspace A writes a hook into the user `settings.json`; the next tool call
in workspace B runs it, inside B's container, where B's broker socket is mounted
(`GET-TOKEN scope=git`, `GET-SECRETS`). The broker is right to answer — the socket is B's — and
socket-as-identity holds; what the shared volume adds is a way for A's code to *be* in B. It reaches
only workspaces that are running (a hook runs in a live Claude Code), and not the App private key,
the secrets master key or the host, which stay behind the chokepoints design §13 keeps — **unless a
running workspace has host access approved**. An approved `privileged` container (docker-in-docker)
is host root (design §6, *What approval means*), so a hook any workspace writes runs there with the
host in reach.

Not measured: whether every one of results 1–4 is picked up by an already-running
`remote-control` server and its sessions without a restart, and which one is fastest. That would
matter for an isolation design; under the adopted model it does not, and phase 2 does not measure
it.

### 2 — `--bg` adds a shared roster

Result 5 puts the daemon's roster, jobs and launch requests on the shared volume while its sockets
and the pids it records are per container. So with `--bg` in use, one container could very likely
launch a session, or act on one, in another container's daemon. That is one more path inside the one
trust domain, not a new boundary crossed. What it *does* raise is a workability question — whether
two containers' daemons on one roster confuse each other — which is phase 2's.

### 3 — The isolation fix considered, and declined

Result 6 suggests a fix: give each container its own `CLAUDE_CONFIG_DIR` and share only the
credential, through `CLAUDE_SECURESTORAGE_CONFIG_DIR` on the volume. Because it moves the credential
and the refresh lock *together*, it would keep Spike 00's cross-container lock — the reason
mitigation (b) in design §7.1 was struck. It would not by itself carry `oauthAccount`, which lives
in `.claude.json` (Spike 02) and Remote Control needs.

**Declined**, for two reasons: the variable is undocumented, and a bump could move or drop it
silently, which is the failure every Claude Code invariant in Drydock exists to catch; and with the
adopted model, isolation is not a goal it would serve. If the model is ever tightened, the
**documented** route is managed settings baked into the image that restrict hooks and MCP servers
(design §15.2), not this variable.

[Spike 09](09-split-credential.md) has since measured the variable for a different reason — `--bg`
per workspace, not isolation — and found the split workable with conditions: the credential, the
refresh lock and the write lock move together, `oauthAccount` must be seeded per container, and a
check run on every bump (its `bump` mode) answers the "a bump could move it silently" objection
above. Not adopted.

### Residual risk, for the operator

A prompt-injected agent in workspace A can likely reach workspace B's repository token and secrets
through a shared hook. Do not run a sensitive repository beside one whose agent reads a lot of
hostile input — issue bodies from strangers, untrusted dependencies, scraped pages. And while a
workspace with host access approved (`privileged`, an `initializeCommand`, a sensitive bind mount,
`--network host`) is running, a prompt-injected agent in *any* workspace can likely reach what that
access reaches: with `privileged`, an `initializeCommand` or a sensitive bind mount, the host, the
App private key and the secrets master key included; with `--network host`, services on the host's
loopback.

---

## Phase 2 — results

Operational viability only; isolation is out of scope. What was asked: the refresh lock with the
daemon (by reading only — a live refresh needs a login of its own, H3); supervisability (a start
Drydock can classify, a stop that deregisters, a way to tell it exited, an id to link from somewhere
stable); environment inheritance (the launching shell's, or the daemon's); two daemons on one roster;
and the lifecycle across container stop and rebuild.

| # | Measurement | Result | Mode |
|---|---|---|---|
| 1 | What the CLI offers | `--bg`; `agents [--json --all]`, `attach`, `logs`, `stop`, `respawn <id>\|--all`, `rm`; `daemon run\|status\|logs\|stop [--any] [--keep-workers]`. *"Service install is disabled in this version — the daemon runs on demand and exits when the last client disconnects."* `daemon run` runs it in the foreground | `cli` |
| 2 | `claude --bg --remote-control <name>` over `docker exec`, no TTY, stdin closed | returns in 0.6 s, exit 0, `backgrounded · <id> (idle — send a prompt to start)`; starts `claude daemon run --origin transient`, which outlives the exec (reparented to PID 1, still serving 70 s later) and exits by itself 5–7 s after its last job stops | `start` |
| 2 | What says it is up, and the link | `sessions/<pid>.json` gains `bridgeSessionId: session_…` (with `kind: bg`, `jobId`, `pidDomain`, `procStart`); `jobs/<id>/state.json` carries the same id as `cse_…` (`state: working`, `tempo: blocked`, `needs: send a prompt to start`). `claude logs <id>` shows `/rc connecting…` and the id in **plain text, not OSC 8**; no `env_…`, no `Capacity`. `claude agents --json` names the job, its pid and local session id, not the bridge session | `start` |
| 3 | Files a start writes under `CLAUDE_CONFIG_DIR` | `daemon.lock`, `daemon.status.json`, `daemon.log`, `daemon/roster.json`, `daemon/control.key`, `jobs/<id>/state.json`, `jobs/pins.json`, `sessions/<pid>.json` (+ `.key`); sockets under `/tmp/cc-daemon-<uid>/` (per container). A job's entry outlives its container (`lifecycle`, result 9); that only `claude rm` removes one is *read* from the help (`cli`) | `start`, `lifecycle`; `cli` *read* |
| 4 | Gates, one removed at a time | **trust:** `claude --bg` refuses at once, exit `1`, *"Workspace not trusted. Run `claude` in /workspace once and accept the trust prompt, then retry."* — no job, no hang. **`remoteDialogSeen`**, **`hasCompletedOnboarding`**: neither needed, Remote Control on. **`oauthAccount`:** the job runs **without Remote Control and says nothing** (`bridgeSessionId: null` everywhere, no line in `claude logs`); the record was back in `.claude.json` within 25 s, written by Claude Code itself | `gates` |
| 5 | Whose environment the session gets | **the daemon's**: the first exec's `DD_MARK=launch1` in both jobs' worker environment and Bash tool shells; a second exec's `DD_MARK=launch2`, `DO_NOT_TRACK=1`, `DISABLE_TELEMETRY=1` reached neither, and its `env -u DISABLE_AUTOUPDATER -u CLAUDE_ENV_FILE` removed neither; `claude respawn` from a third exec (`launch3`) kept `launch1`. Workers add `CLAUDECODE=1`, `CLAUDE_CODE_SESSION_KIND=bg` | `env` |
| 5 | `CLAUDE_ENV_FILE` | ran before the Bash call in both jobs (one marker line each, `DD_MARK=launch1`) — including the job whose launching exec had unset it | `env` |
| 6 | `claude stop <id>` | graceful: `teardown … reason=host_exit`, `Archive cse_… status=200`, `Torn down (archive=200)`; `daemon.log` says `settled <id> (killed)` | `stop` |
| 6 | `claude respawn <id>` after a stop | the **same** session: `Reattaching to session …`, `Unarchive … status=200` | `stop` |
| 6 | `SIGTERM` of the worker | the same graceful archive; then, unasked, a new worker about 11 s later with a **new** session (`Created session cse_…`); nothing in `daemon.log` | `stop` |
| 6 | `SIGKILL` of the worker, `respawn` at once | the same session, `Unarchive … status=409` (it had not been archived), no refusal and no wait — nothing like Spike 02's *already served* lockout. `state.json` read `SIGKILL (137); respawning` (whether the daemon respawns unasked was not separated from the harness's own `respawn`; Spike 06 saw it not) | `stop` |
| 6 | `SIGTERM` of the transient daemon | the daemon exits (`cause=signal, live_workers=1`); **the job's worker survives**, orphaned to PID 1, still serving; the next `claude --bg` starts a daemon that adopts it (`adopted=1`) | `stop` |
| 7 | `claude daemon run` held by a `docker exec` | `origin: foreground`, the exec's process; jobs run under it; it does **not** idle-exit | `daemon` |
| 7 | `claude daemon stop` / `--any` | without `--any`: refused (*"a daemon is running (pid=9, origin=foreground). Run `claude daemon stop --any`"*); with it: `stopped (terminated 1 background session)`, the session archived (`archive=200`), the daemon and the holding exec end | `daemon` |
| 7 | `SIGTERM` of the foreground daemon | as for the transient one: the daemon exits, the worker is orphaned and keeps serving | `daemon` |
| 8 | B, before running anything, with A's job live | `claude daemon status`: *"supervisor not running but 1 worker in roster — running `claude agents` restarts the daemon and re-adopts …"*; `claude agents --all` lists A's job; `claude logs <A's id>`: `connect ENOENT …/control.sock` | `two` |
| 8 | B starts a job, **both clones at `/workspace`** | **the job runs in container A** (its registry row's `pidDomain` is A's pid namespace) under A's daemon; B's daemon starts too, tries the same job and records `settled <id> (crashed): session ID already belongs to another conversation`, marks A's live job dead (`adopt: … dead=1`), idle-exits and rewrites the roster with no workers. Both containers then list **both** jobs as `failed`, while both serve Remote Control in A | `two` |
| 8 | Same, **clones at `/workspaces/ws-a`, `/workspaces/ws-b`** | the job runs in B: a daemon logs `refused <id> (shell): working directory no longer exists or is not accessible: /workspaces/ws-b` and the other takes it ~5 s later. A's live job is still marked dead and listed `failed` in both | `DISTINCT=1 two` |
| 8 | B: `claude stop <A's job>` | prints `stopped <id>`; A's worker untouched, still serving | `two`, `DISTINCT=1 two` |
| 8 | B: `claude respawn <A's job>` | same folder: a **second worker in B** on the same conversation and the same app session, beside A's; distinct folders: refused, *"Workspace not trusted. /workspaces/ws-a could not be resolved on disk."* | `two`, `DISTINCT=1 two` |
| 9 | `docker stop`, and `docker rm -f`, with a job live | immediate either way, and **no teardown** and no archive: `docker stop` SIGTERMs PID 1, which exits at once, and its exit has the kernel SIGKILL the rest; `docker rm -f` SIGKILLs at once. Left: `daemon.lock`, `daemon.status.json`, the roster's worker, `state.json` still `working`, the `sessions/` row | `lifecycle` |
| 9 | The container back (`start`, or a new one) | no daemon until something asks; `daemon status` warns as in 8; `claude agents` lists the job `failed` and did not start a daemon; a new `--bg` works beside the stale entry (`adopt: … dead=1`, `orphan-spare reap: 1`); `respawn` of the old job reattaches **the same session** (`Unarchive … 409`) with no refusal | `lifecycle` |
| 10 | The daemon's refresh | schedules `expiresAt − 240 s` (bounded to 5 s … 24 h; *measured* by `start`'s explicit `grep -a 'scheduling proactive refresh' daemon.log`: `scheduling proactive refresh in 11583s` with 197 min left), and calls `checkAndRefreshOAuthTokenIfNeeded` from Claude Code's auth chunk — the CLI's own refresh, exported beside `beginOAuthRefreshLockHold`; the lock is `<dir>/.oauth_refresh.lock` with `stale: 60000`, `update: 5000` (Spike 00's values) plus a `.oauth_refresh.lock.owner` record of `pid`, `procStart`, `pidDomain`. After another process's refresh it logs `token still valid (cross-process refresh or not yet due)`; it hands the token to its workers (`token_update`) | `refresh`, *read*; the scheduling line `start` |

### 1 — Inside one container it is supervisable, by other means than the server's

A start is one `docker exec` that returns at once with an id (result 2), so Drydock's supervisor
would no longer hold the session's PTY: the process it would hold is the daemon. Held as `claude
daemon run` (result 7), the daemon is an ordinary foreground process that does not idle-exit, and the
supervisor would stop the workspace's session with `claude daemon stop --any`, which archives it —
the counterpart of the server's `SIGTERM` that deregisters. `SIGTERM` of the daemon is **not** a stop:
the worker outlives it, orphaned (results 6, 7). A `SIGTERM` of the worker is not a clean stop either:
something restarts it about 11 s later with a *new* app session, so a link Drydock had shown goes
dead (result 6). `claude respawn <id>` is the restart that keeps the session, after a stop, a crash or
a container restart (results 6, 9); and nothing here waits out a lapsing registration — the
*already served* refusal that drives §8's `409` state did not appear.

The link comes from a file, not the terminal: `sessions/<pid>.json`'s `bridgeSessionId` (or
`jobs/<id>/state.json`'s, as `cse_…`). `claude logs` prints the id as plain text, so the OSC 8 rule
Drydock scrapes by would find nothing (result 2). On the shared volume a `sessions/` row must be read
with its `pidDomain` and `procStart`, since pids overlap between containers and a new container can
reuse a removed one's pid namespace (harness trap, `lifecycle`). And it is one conversation:
`claude.ai/code/session_<id>`, no environment, no `Capacity` (Spike 06 results 6 and 10).

The gates differ from the server's. A missing trust record is a clean refusal with an exit status and
a sentence, not a hang (result 4) — better than the server. A missing `oauthAccount` is worse: no
error anywhere, a session that silently lacks Remote Control, detectable only as a `bridgeSessionId`
that never appears.

### 2 — The environment is the daemon's, frozen when it starts

Every job gets the environment of whichever exec started the daemon, and keeps it across `respawn`
(result 5). The daemon also pre-spawns a spare worker at start (`bg spare spawned`), which a later
job claims. So the property §8 relies on — secrets loaded before `exec`, inherited by every session —
holds only if Drydock's own exec, after the `drydock-secrets export` prelude, is what starts the
daemon, and a change of secrets needs the daemon restarted. If the operator's or the agent's own
`claude --bg` starts it first, every later job in that container gets that shell's environment. The
must-stay-unset variables are safe in the same way and for the same reason: a later launcher's
`DO_NOT_TRACK=1` never reached a session. `CLAUDE_ENV_FILE` is the container's and ran (result 5), so
the secrets prelude itself works under `--bg`.

### 3 — Two daemons on one config confuse each other

This is what decides it. Each container runs its own daemon (its sockets are in its own `/tmp`), but
both read and write the one `jobs/` directory and roster on the volume (result 8):

- **With the same workspace folder in both containers, a job launched in B ran in A**, in A's clone
  and beside A's broker socket, and B's own daemon crashed trying the same job. A Drydock workspace's
  folder is the devcontainer CLI's default, `/workspaces/<clone folder>` — the repository's name
  (design §6, step 2, measured on CLI 0.89.0) — unless its `devcontainer.json` sets
  `workspaceFolder`, and many pin one (`/workspace` is common). So any two running workspaces whose
  repositories pin the same `workspaceFolder` are exactly this case, as are two of repositories that
  share a name (different owners, or a fork).
- **With distinct folders, the job runs where it was launched**, after a refusal by the other daemon
  and a delay of about five seconds. But the other daemon still marked A's live job dead, so
  `claude agents` showed a session that was serving as `failed`, in both containers.
- **A command run in the wrong container lies.** `claude stop` of another container's job prints
  `stopped` and stops nothing; `claude respawn` of it, with a shared folder, starts a second worker on
  the same conversation and app session.

None of this is a security finding under the adopted model; it is state that cannot be trusted. A
supervisor could work around it — never read `claude agents`, act on a job only from its own
container, track its own ids — but the job-in-the-wrong-container case it cannot prevent from
outside, short of refusing two workspaces with one folder name. 2.1.289 offers no documented way to
give a daemon its own `jobs/` or roster on a shared `CLAUDE_CONFIG_DIR`.

### 4 — Container stop and rebuild leave stale state that does not block

`docker stop` kills the session without a teardown (result 9), so the app session is not archived —
the reverse of a `claude stop`. What is left on the volume is read as dead by the next daemon, and
neither blocks a new job nor stops `respawn` from reattaching the old session. It accumulates:
`jobs/` keeps every job of every workspace until `claude rm`, and every container lists them all.

### 5 — The refresh: the CLI's own path, by reading

The daemon refreshes on its own timer, four minutes before `expiresAt`, through the same refresh
function the CLI uses, and the lock that function takes is Spike 00's `mkdir` lock with the same
60 s staleness and 5 s update, now with an owner record naming the holder's pid namespace (result 10,
*read*). On one volume, every container running a daemon is one more refresher on the same schedule
— the daemons would all wake within the same few seconds. That reading says it should serialise like
every other refresher, but it is a reading: H3 is the live check, and it needs a login of its own.

### What Drydock would have to change to adopt it

Not recommended (the verdict); recorded so the cost is on file.

- **`internal/supervisor`:** hold `claude daemon run` (not the session) under `devcontainer exec`,
  after the secrets prelude; start the session with `claude --bg --remote-control <prefix>` and parse
  `backgrounded · <id>`; read the link from `sessions/<pid>.json` (matched by `pidDomain` and
  `procStart`) or `jobs/<id>/state.json`, not from OSC 8; stop with `claude daemon stop --any`;
  restart with `claude respawn <id>`; re-read the session id after any unexplained worker restart; a
  secrets change is a daemon restart. Never trust `claude agents`. The card links one session, with
  no count and no way for the app to open another (Spike 06's price).
- **`internal/classify`:** new signatures — the trust refusal is `claude --bg`'s, exit `1`; a missing
  `oauthAccount` has none (a timeout on `bridgeSessionId`); and the daemon's own
  `settled <id> (crashed): session ID already belongs to another conversation` and `refused <id>
  (shell): working directory no longer exists`, which appear only in the shared `daemon.log`, whose
  lines carry no pid. The corpus would need recording for all of them.
- **The Feature:** the trust record stays; `remoteDialogSeen` would no longer be needed for this
  path; and a guarantee that no two running containers share a workspace folder — or per-container
  daemon state, which 2.1.289 does not offer.
- **The expiry watch (§7.3):** one more scheduled refresher per running workspace; H3 first.

## Owed (human steps)

Written as steps in the [harness README](harness-08-bg-rc/README.md#the-human-steps):

- **H1** — the `--bg` session in the app: where it is listed, its title, whether a turn sent from the
  app is answered (`WAIT_APP=300 ./run.sh app`).
- **H2** — what the app shows after `claude stop` (archived, offline, gone) and after `claude
  respawn` (back, with its history).
- **H3** — the daemon's refresh live against Spike 00's lock: two containers' daemons and a
  `remote-control` server on one volume, on a **disposable login the owner makes for the run** —
  never a copied credential.

## Open questions

- Whether `claude daemon run [json-path]` / `--json-path` can move the roster and `jobs/` off the
  shared directory (not tried; the help names only `daemon.json` and the log), which would be the
  condition result 8 lacks.
- Whether a running `remote-control` server picks up a hook or MCP server written after it started
  (result 1's *not measured*), which would only matter if the model is tightened.
