# Spike 09 — A per-container config with one shared credential

**Question.** [Spike 08](08-bg-rc.md) found `claude --bg --rc` supervisable inside one container but
not workable as Drydock's per-workspace model, because every container's daemon reads and writes one
roster and one `jobs/` in the shared `CLAUDE_CONFIG_DIR` (design §7.1): a job launched in B ran in A,
and live jobs were reported dead. Its phase 1 read a way out of the binary and declined it:
`CLAUDE_SECURESTORAGE_CONFIG_DIR`, undocumented ([anthropics/claude-code#79223](https://github.com/anthropics/claude-code/issues/79223)),
which moves `.credentials.json` and the refresh lock to a directory of their own. This spike asks
whether that *split* layout works: each container its own `CLAUDE_CONFIG_DIR` (its own volume per
workspace), only the credential shared through the variable, and `oauthAccount` seeded into each
container's `.claude.json` — so that one `claude --bg --rc` conversation per container runs, the
phone uses it through the Claude app, and the laptop joins it with `claude attach`. The owner's use
case: create a workspace from the phone, explore from the phone, build out from the laptop in VS
Code, one live conversation throughout. It would sit behind a flag, `shared | split`, with today's
`shared` layout as the fallback.

**Status: measured, but for two human steps (H1, the app and laptop on one conversation; H2, a live
refresh on a disposable login).**

**Verdict: workable, with conditions.** Under the variable the credential, its refresh lock (and the
lock's owner record) and its write lock all live in the one shared directory, while `.claude.json`
— the account record, trust, the daemon's roster and jobs — stays per container (results 1, 7).
Refreshers of three kinds in four containers, started at once, found that one lock path: two were
seen holding it, one after the other, and none took a lock anywhere else (result 6, with the control
that without the variable each container took its own, all four at once). Spike 08's
collision is gone: two containers, each with its own daemon, at the same `/workspace` or at distinct
folders, each see and act on only their own jobs (result 3). A seeded container's `--bg --rc` gets
Remote Control on its first start, and a terminal's `claude attach` drives the conversation the app is
sent (results 2, 4). The conditions:

1. **The bump check passes on every Claude Code bump** (`./run.sh bump`, result 7), and `shared`
   stays the default and the fallback. The variable is undocumented; the check is what makes a flag
   on it safe across versions.
2. **`oauthAccount` is in each workspace's own `.claude.json` before its first start.** Unseeded,
   `--bg --rc` runs *without Remote Control and says nothing*, and today's `remote-control` server
   refuses with the organization error, writes no record, and refused the same way when stopped and
   started again (result 2). So Drydock becomes the second writer of `oauthAccount` — copied from the
   login's record, under `.claude.json.lock`.
3. **Every process that reads the credential has the variable**: the container's environment (so a
   VS Code terminal's `claude attach` has it too — set only under `split`, see *The flag* below), the
   identity watch, the login. Without it, a per-container config holds no credential and reads as
   signed out.
4. **H2 before the flag ships, H1 before it is offered.** The lock reading and the fake-credential
   measurements say the refresh serialises; only a live refresh on a login of its own shows the losers
   pick up the winner's token.

- **Verified against:** Claude Code `2.1.289` (the pinned version; the host's binary, bind-mounted
  read-only into each container; `claude --version` printed first by every mode), Linux,
  docker-in-docker, scratch Debian 12 containers run as `devcontainer up` runs one (PID 1 a sleep
  loop, as the CLI's is), every command a `docker exec` as uid 1000. Not the Feature's install, the
  broker, or `devcontainer up` itself. Each result is *measured* unless marked *read* or *inferred*,
  and names its harness mode.
- **Date:** 2026-10-31.
- **Harness:** [`harness-09-split-credential/`](harness-09-split-credential/) — re-runnable; each
  result names its mode; H1 and H2 are written there as human steps. The lock, tombstone and bump
  modes run on Spike 00's **fake** credential, never the owner's; the rest run on a copy of the
  owner's under Spike 08's token guard, each well inside the token's lifetime (the guard reported
  194–200 min left against the 70 it needs). Every real-credential mode records the copy's SHA-256
  when the guard passes and compares it at exit (`cred_check`): unchanged in all six (`seed`,
  `server`, `two`, `DISTINCT=1 two`, `attach`, `supervise`), so no run refreshed it. Each
  container's PID 1 also exits by itself 30 min before the copy's `expiresAt` (the guard's
  deadline), ahead of the proactive refresh a daemon schedules about 4 min before it (`seed`:
  `scheduling proactive refresh in 11779s` with ~200 min left), so a killed run that is never swept
  cannot reach that refresh. Spike 08's harness has the same residual without this deadline (its
  PID 1 loops until stopped); it is not changed here.

---

## Results

| # | Measurement | Result | Mode |
|---|---|---|---|
| 1 | Where the credential is read from | `<variable>/.credentials.json`; one in `CLAUDE_CONFIG_DIR` is not read while the variable is set (signed out with it there and the variable's dir empty) | `bump` |
| 1 | Where a refresh locks | `<variable>/.oauth_refresh.lock` (a directory) and `<variable>/.oauth_refresh.lock.owner` (`pid`, `procStart`, `pidDomain`); nothing in any `CLAUDE_CONFIG_DIR`. The owner record outlives the lock until the next holder rewrites it | `lock`, `bump` |
| 1 | The legacy refresh lock | `<realpath(variable)>.lock`, **beside** the dir, so in each container's own filesystem (`/home/vscode/.claude-credentials.lock`), held by a container exactly while it held the shared lock (a: 862 of 1200 samples 50 ms apart, c: 201; b, d: 0). Without the variable it is `/home/vscode/.claude.lock` — per container in both layouts | `lock`, `NOVAR=1 lock` |
| 1 | The credential's write lock | `<variable>/.storage-write.lock`: held there by the harness, the refresh that was refused kept its refresh lock 7.6 s waiting and **the credential was not blanked**; the control blanked it ~80 ms after taking the refresh lock. Too brief to see by polling | `HOLD=1 blank`, `blank` |
| 1 | The tombstone | lands in the shared dir: `accessToken`/`refreshToken` `""`, `expiresAt` `0`; both containers then `loggedIn: false` — fleet-wide, as today | `blank` |
| 1 | What stays per container | `.claude.json` (the account record: `auth status` reports the `orgId` seeded into the config, not that of a decoy record planted in the variable's dir, which nothing reads or writes; and nothing creates a `.claude.json` there), `daemon/`, `jobs/`, `sessions/`, `daemon.log`, `backups/`, `cache/`; each config mints its own `userID` and `machineID`. After three daemons ran on it for a minute, the shared dir held `.credentials.json` alone and no config held a credential | `bump`, `seed` |
| 2 | `--bg --rc`, `oauthAccount` seeded | Remote Control on the first start (`bridgeSessionId` within 30 s). Claude Code rewrote the seeded record at start: `profileFetchedAt` from the source's (37 h old) to the start time, every other field kept | `seed` |
| 2 | `--bg --rc`, not seeded (the control) | the job runs **without Remote Control and says nothing** (`bridgeSessionId: null`), as in Spike 08; Claude Code wrote a record of its own during the run (without the login's `organizationRole` and `workspaceRole`), and `claude stop` + `claude respawn`, or a new job, then had Remote Control | `seed` |
| 2 | `--bg --rc`, record absent, `CLAUDE_CODE_ACCOUNT_UUID`, `_USER_EMAIL`, `_ORGANIZATION_UUID` set | Remote Control on the first start; the record written from them, then from the profile | `seed` |
| 2 | Today's server (`claude remote-control --spawn worktree --capacity 4 --verbose`), seeded | an environment id, `Capacity: 0/4` → `1/4`; the record refreshed as above | `server` |
| 2 | The same, not seeded | `Error: Unable to determine your organization for Remote Control eligibility`; **no record written** | `server` |
| 2 | Both, `SIGTERM`ed and started again | the unseeded one refused the same way and still wrote no record; the seeded one came back on the **same** environment id, `Capacity: 0/4` → `1/4`. One restart measured, not a restart loop | `server` |
| 3 | B, before running anything, with A's job live | `daemon status`: `not running`, 0 workers; `agents --all`: empty; `logs <A's id>`: `No job matching` — B sees nothing of A | `two`, `DISTINCT=1 two` |
| 3 | Both start a job, **both at `/workspace`**, and with distinct folders | each runs in its own container under its own daemon, each with its own Remote Control session, still serving 30 s later; each lists only its own; neither `daemon.log` names the other's job | `two`, `DISTINCT=1 two` |
| 3 | B: `claude stop` / `respawn` of A's job | `No job matching '<id>'`; A's job untouched | `two`, `DISTINCT=1 two` |
| 3 | A: `stop`, then `respawn`, of its own | only A's: B's job kept its worker and session; A's came back on the **same** session; both daemons exited within 7 s of their last job | `two`, `DISTINCT=1 two` |
| 4 | `claude attach <id>` from a second exec (a TTY), one turn typed there | answered (`TERM1`) in the terminal; the transcript in the container's own config has the user turn and the reply; the worker's bridge logged `Sending 1 message(s)` twice and `Sent result success … num_turns=1`. Killing the attach client left the job serving on the same session. That the app shows the turn is H1 | `attach` |
| 5 | Supervision under split | as Spike 08 result for result: `claude daemon run` held by an exec is `origin: foreground`; the link is `sessions/<pid>.json`'s `bridgeSessionId` (= `jobs/<id>/state.json`'s `cse_…`); `claude stop` archives (`Archive … status=200`, `Torn down`); `respawn` reattaches the **same** session (`Unarchive … 200`); `docker stop` runs no teardown, leaves state in the container's own config only and nothing in the shared dir; after `docker start` no daemon runs until asked, and `respawn` reattaches the same session (`Unarchive … 409`); `daemon stop --any` archives | `supervise` |
| 6 | The resolver | `HS()`: the variable if set (an empty value means `~/.claude`), else the config dir. The plaintext store (`i()`), the credential write lock (`.storage-write`), the refresh (`let K=HS()` … `.oauth_refresh.lock`, `stale: 60000`, `update: 5000`), the legacy lock (`<realpath(K)>.lock`) and the credential's change watch (`mtime` of `HS()/.credentials.json`) all go through it. `--bg`'s job environment allowlist carries the variable to workers, kept even when empty | `read`, *read* |
| 6 | A reader that does not | a session-store resume path (`SessionStore.load()`) copies `$CLAUDE_CONFIG_DIR/.credentials.json` directly. Not on the `--bg` or `remote-control` path; under split it would find no credential | `read`, *read* |
| 6 | The account record's upkeep | refetched when `profileFetchedAt` is older than `RK` = 86 400 000 ms (24 h) or a field is missing; seeded from the three `CLAUDE_CODE_*` variables when absent | `read`, *read* |
| 6 | The organization lookup most API calls use (`NW`) | `CLAUDE_CODE_ORGANIZATION_UUID` if set, then the record's `organizationUuid`, then the profile endpoint — the variable before the record | `read`, *read* |
| 6 | A refresher that finds the lock held | `ELOCKED`: up to five retries 1–2 s apart, then a dead-holder check. Its only output is an analytics event, not a debug line | `read`, *read* |
| 6 | Four refreshers in four containers at once (two `claude -p`, a `remote-control` server, a `--bg` daemon), expired fake credential, token host blackholed | **one lock path, two holders seen, one after the other, and no lock anywhere else**: c's server took `<variable>/.oauth_refresh.lock` at 1.0 s and released it at 11.1 s; a's `-p` took it at 11.6 s, held it to 41.6 s and took it again at once; b never held it; d's daemon began its scheduled refresh inside c's hold and ended in 2.1 s without holding it. No config dir held a refresh lock (d's held only the daemon's `daemon.lock`). No debug log has an `ELOCKED` or retry line (the retry is not logged; above), so that b waited on the lock rather than on something else is *inferred* | `lock` |
| 6 | The same without the variable (the control) | **four locks at once**: at 1.0 s each container held `.oauth_refresh.lock` in its own config dir — the struck mitigation (b) of design §7.1, measured | `NOVAR=1 lock` |
| 6 | The daemon's refresh | `auth: proactive refresh failed, signalling re-auth required`, then `headless daemon cannot complete OAuth — run claude auth login to refresh` and `no token found, will re-check keychain every 30s`, about 2 s after it started (2.1 s, 1.8 s); it spawned its job only after that. The same in both layouts, and in the control d's config held a refresh lock of its own, so it is **not** a reaction to a busy lock and not split's. Whether it recovers when a peer's refresh succeeds is H2 | `lock`, `NOVAR=1 lock` |
| 7 | The bump-ritual check | 13 assertions: the variable in the binary; two tripwires (the write lock's name, the `--bg` allowlist's case for the variable) and their control; the credential read from the variable's dir and not from the config dir; the account record from the config dir, with a decoy record of another organization in the variable's dir left unread and untouched; the refresh lock in the variable's dir and no lock in the config dir; the watch sees a lock without the variable; and the shared layout as one dir mounted at both paths signs in and locks beside its credential. `BUMP CHECK PASSED`, exit 0 | `bump` |
| 7 | The bump check, mutated: the variable misspelled in every container | **four fail**, exit 1: signed out with the credential in the variable's dir; signed in from the config dir's; `orgId` absent (signed out, so not the seeded one); no refresh lock in the variable's dir. The other nine pass: the binary's, the tripwires' three, the decoy's, no lock in the config dir, the watch's control and the one-dir pair | `BUMP_MUTATE=var bump` |
| 7 | The bump check, mutated: the refresh lock moved, the credential not | a copy of the binary with the lock's name patched, same length, to `../.claude/o_r.lock` (both copies of the constant, source and bytecode): the refresh locked `cfg/o_r.lock` while its `.owner` record stayed in the variable's dir. **Exactly the two lock-location assertions fail**, exit 1: no refresh lock in the variable's dir, a lock in the config dir. The other eleven pass | `BUMP_MUTATE=lock bump` |

### 1 — The split is clean: the credential and its locks move together, the rest stays

What the variable moves is exactly what has to be shared: the credential, the refresh lock and its
owner record, and the credential's write lock (results 1, 6). The refresh critical section Spike 00
measured — re-read, lock, re-read, refresh, write through `.storage-write`, release — happens entirely
in the one directory every container mounts, so its cross-container exclusion holds as it does on
today's shared volume. Everything else stays in the container's own config: the account record,
trust, history, settings, and the `--bg` daemon's roster and jobs.

The one lock that does not move with it is the *legacy* lock, `<dir>.lock`, which sits beside the
directory. It is per container in both layouts (today's is `/home/vscode/.claude.lock`, outside the
volume), so it never was the cross-container lock; the current lock is. A mount point whose parent the
remote user cannot write would only make its acquisition fail, which the binary logs and goes on from
(*read*); `/home/vscode/.claude-credentials`, under the user's home, avoids even that.

### 2 — `oauthAccount` must be seeded, and the server will not seed itself

Spike 02's gate holds per container: a config with no account record has no Remote Control. Under
`--bg` that is silent — a job that runs, a session the app never sees — and Claude Code writes a record
during that first run, so the *next* start has Remote Control (result 2). The server Drydock runs today
writes nothing: it refuses with the organization error, and stopped and started again it refused the
same way, still without a record (result 2; one restart measured). Drydock's supervisor classifies that
refusal as `awaiting_login`, and a sign-in writes the login's config, not this one, so a sign-in would
not clear it for that container (*inferred*: nothing in the server's path writes a record, and a
restart did not). Neither is a basis for a product; a seed is.

The seed is one object, copied from the login's own `.claude.json` into the workspace's, merged under
Claude Code's `.claude.json.lock` the way the Feature already writes trust (`seed-oauth.sh`). Once
seeded, Claude Code keeps it: it refetched the profile at the first start because the copy was a day
and a half old, and keeps it fresh on a 24-hour clock (results 2, 6). So Drydock seeds once per account,
not per start; a re-login as a *different* account needs a re-seed, which the 24-hour refetch would
otherwise leave stale for up to a day. The three `CLAUDE_CODE_*` variables are a second way in
(result 2) that writes nothing under a lock, but they are fixed in the container's environment until a
rebuild, and the organization lookup most API calls use returns the variable before the record
(result 6, `read`), so a changed account would stay wrong there. The seed is the one to build.

### 3 — Spike 08's collision is gone

With its own config, each container's daemon has its own roster and `jobs/`, so there is nothing for
the other to run, adopt, reap or misreport. Both of Spike 08's failing cases — the same workspace folder
in two containers and distinct ones — now behave as one container did in Spike 08: each lists its own
job, the other's id is unknown (`No job matching`), stop and respawn reach only their own (result 3).
That removes the condition Spike 08 could not meet from outside ("refuse two workspaces with one
folder"), and the "never trust `claude agents`" rule with it.

### 4 — The terminal joins the conversation the app has

`claude attach` in a second shell of the container is a full client of the background session: a turn
typed there is answered, recorded in the session's transcript, and sent over the worker's Remote
Control bridge, and a detach leaves the session serving (result 4). This is the laptop's half of the
owner's use case; in VS Code the terminal is an attached container window's (Drydock's *Open in VS
Code* link, `internal/vscode`), and it has the variable only if the container's environment does (condition 3). The phone's
half — that the app shows the terminal's turn in the same conversation, and that a turn sent from the
app reaches the terminal — is H1. The app side has been measured for a terminal's `claude --rc`
session (Spike 06 result 6, its H2) but not yet for a `--bg` one (Spike 08's H1 is owed too); nothing
in the split should change what the app sees, but the use case is what the flag is for, so H1 checks
it end to end.

### 5 — Supervision is Spike 08's, without its caveats

Every supervision result of Spike 08 reproduces under split (result 5), and three of its workarounds
fall away: a `sessions/` row need not be matched by `pidDomain` (only this container writes it),
`claude agents` can be believed, and stale jobs of other workspaces no longer accumulate in every
listing. What remains is Spike 08's cost list: the daemon, not the session, is the process the
supervisor holds; the environment is the daemon's, frozen at its start; one conversation per process,
with no environment id and no `Capacity` for the app to start another in.

### 6 — The refresh serialises on one lock (read, and fake-measured); live is H2

Reading the binary, every credential path goes through one resolver, and the refresh, its lock and the
write lock all resolve to the variable's dir (result 6). Measuring with a fake credential, four
refreshers of three kinds — the CLI's own (`-p`), a `remote-control` server and a `--bg` daemon — in
four containers, started at once, used only that one path: two were seen holding it, the server and
then a `-p`, never together, and no config dir held a refresh lock, while without the variable each
container held its own at the same instant. What the other two did is less direct: the second `-p`
never held the lock and logged nothing about it (the `ELOCKED` retry loop emits only an analytics
event, `read`), and the daemon ended its refresh in two seconds. Spike 00's exclusion rests on
`mkdir(2)` of one path, and that path is now the variable's dir, so it holds, on the same condition (a
local filesystem).

What the fake cannot show is a refresh that *succeeds*: whether a daemon picks up a peer's token. The
daemon's own refresh gave up within about two seconds and announced *re-auth required*, re-checking
every 30 s (result 6) — in both layouts, and in the control while its config held a refresh lock of
its own, so it is not a reaction to a busy lock but a fact about `--bg` on an unreachable token
endpoint, which the shared layout's daemons would meet too. Spike 08's reading has the daemon log
`token still valid (cross-process refresh …)` after another process's refresh, which says it recovers;
H2 is the live check, on a login of its own, and is owed before the flag ships.

### 7 — The bump check

`./run.sh bump` is the check the §11.1 ritual would run whenever `split` is in use. It needs no login
and no network beyond a blackholed host: a fake credential, two containers, a minute. It asserts that
the variable still moves the credential, that the refresh lock still follows it (and does not stay in
`CLAUDE_CONFIG_DIR`), that the account record still comes from `CLAUDE_CONFIG_DIR` even with a decoy
record of another organization in the variable's dir, and that the shared layout, expressed as one
volume mounted at both paths, still keeps them together (result 7).

It is mutation-checked, and the mutations are modes, so the check can be watched failing again on a
new version. `BUMP_MUTATE=var` misspells the variable in every container — Claude Code ignoring it —
and four assertions fail. `BUMP_MUTATE=lock` is the silent failure the check exists for: a bump that
moves the refresh lock away from the credential, which would give each refresher its own lock again.
It is a copy of the binary with the lock's name patched, at equal length, to climb from the variable's
dir into the config dir; the credential and the lock's `.owner` record stay put, and exactly the two
lock-location assertions fail (result 7). Either way it exits 1, and `split` falls back to `shared`.

Two things it does not test by behaviour. The credential's write lock (`.storage-write`) and the
`--bg` job allowlist's case for the variable are **tripwires**: the check fails if their text leaves
the binary, but a bump that keeps the text and changes what it does passes. Their behaviour is measured
by `HOLD=1 blank` (the write lock) and by `seed` and `two` (workers of `--bg` jobs signed in from the
shared credential), which the bump ritual re-runs only when a tripwire trips or the refresh path
changes.

---

## What Drydock would change

Not adopted by this spike; recorded so the decision has its cost.

**The flag.** `--claude-layout shared|split` (config; `shared` the default), held for the instance:
changing it means rebuilding every workspace, since mounts and the container's environment are fixed
at `up`. `shared` must stay byte-for-byte today's layout, variable included: today no container has
`CLAUDE_SECURESTORAGE_CONFIG_DIR`, and a `shared` that set it would be a different layout (one volume
at both paths, which `bump` step 5 measures) resting on the undocumented variable — the very thing the
fallback is for. So the variable is set **only under `split`**, and not by the Feature (its
`containerEnv` cannot take an option): the docker guard's policy carries it, and `WithAdded` adds
`-e CLAUDE_SECURESTORAGE_CONFIG_DIR=/home/vscode/.claude-credentials` (a constant on both sides, like
`container.ClaudeConfigMountPoint`) right after `run`/`create`, as it adds the policy's name and
labels today. A variable set at create is in the container's own config, so every `docker exec` —
`devcontainer exec`, the supervisor's server, VS Code's attached window and its terminals — inherits
it (VS Code's is *inferred*: it attaches by `docker exec`; not measured here). A Compose workspace gets
neither name nor labels from the guard today, so `split` refuses one until it has a route of its own.
`split` then mounts `drydock-claude-credentials` at the variable's path and a volume of the
workspace's own at `CLAUDE_CONFIG_DIR`; `shared` mounts `drydock-claude-config` at
`CLAUDE_CONFIG_DIR` alone, as now. Allowed only for the Claude Code version the bump check passed on
(a constant checked against `classify.ClaudeCodeVersion`). Falling back costs one sign-in and each
workspace's own history: the credential is never copied between layouts. (Had the Feature set the
variable unconditionally, `shared` would be step 5's one-dir layout, which `bump` does cover; but
then a bump that broke the variable would break `shared` too.)

- **§6 step 4 (`internal/container`, `internal/provision`):** under `split`, make the credential
  volume as today's is made (local driver, the label, Drydock's uid as owner, refused if foreign or
  networked) and a per-workspace config volume the same way, labelled with the workspace, removed with
  the workspace (by label; the credential volume, like today's, by nothing). Two mounts in `up`.
- **`internal/dockerguard`:** the policy's environment entry, added by `WithAdded` and refused, like a
  label, if the argv already sets it to anything else.
- **The Feature:** no variable of its own; `drydock-claude-config` also writes
  `hasCompletedOnboarding` and `theme` (without them a fresh config stops an interactive `claude` at
  the first-run theme picker, Spike 06 result 8; this spike seeded both, so `attach` without them is
  not measured) and seeds `oauthAccount` from the login's record when the config has
  none or has another `accountUuid` — the object alone, under `.claude.json.lock`, never invented and
  never edited; `drydock-preflight` refuses a network filesystem at the variable's dir as well as at
  `CLAUDE_CONFIG_DIR`, and refuses a container whose `CLAUDE_SECURESTORAGE_CONFIG_DIR` is unset or
  holds no credential while `split` is in force. **Bump its `version`.**
- **The login (`internal/login`, §7.2):** the login container mounts the credential volume at the
  variable's path, with the variable set, and a `CLAUDE_CONFIG_DIR` that keeps the login's
  `.claude.json` — a `drydock-claude-login` volume, or a subdirectory of the credential volume — as the
  seed's source. After a sign-in, a re-seed is owed to every workspace whose record names another
  account **or has none**. `drydock-claude-config` runs as a `postCreateCommand`, once per container,
  so a running container needs Drydock to write it: the identity watch's `OnChange` (as it calls
  `ResumeAwaitingLogin` today) launches a provision job per running workspace that runs the Feature's
  seed in it with `devcontainer exec` — the route the supervisor already uses into the container —
  and only then resumes a server parked on the organization refusal. A stopped workspace is seeded at
  its next start, before its server, by the same exec.
- **The expiry watch (`internal/identity`, §7.3):** read `.credentials.json` from the credential
  volume, and run `auth status --json` with the variable set — without it every read is `absent`.
  `blanked` is unchanged (result 1: the tombstone is fleet-wide in both layouts). H2 decides whether a
  daemon that loses the refresh race needs anything from Drydock.
- **`internal/supervisor` (§8):** under `split` with today's server, unchanged — but an organization
  refusal from a container whose config was never seeded is a seeding fault, not `awaiting_login`,
  and must say so. If the supervisor moves to `--bg` for the owner's use case: Spike 08's list (hold
  `claude daemon run` after the secrets prelude, start with `claude --bg --remote-control`, link from
  `sessions/<pid>.json`, stop with `claude daemon stop --any`, restart with `claude respawn`), without
  its shared-roster caveats; the card links one session, with no count.
- **`internal/classify`:** the `--bg` signatures Spike 08 listed, less the two that came from the
  shared `daemon.log` (`session ID already belongs to another conversation`, `working directory no
  longer exists`); plus the daemon's `headless daemon cannot complete OAuth`, which result 6 shows is
  not always a dead login.
- **The bump ritual (testing §11.1):** add `./run.sh bump` to the harnesses re-run on every bump while
  `split` exists — with `BUMP_MUTATE=var` and `BUMP_MUTATE=lock`, each of which must exit 1 with the
  assertions result 7 names (`lock` refuses, and says so, when the lock's name is gone from the
  binary; then the check itself has already failed) — and H2 once per bump that changes the refresh
  path.

**CLAUDE.md invariants**, if `split` is adopted:

- *`.credentials.json` alone is not enough*: unchanged as a fact, but "`oauthAccount` comes only from a
  login, and the Feature never writes it" gains its one exception — under `split` the Feature copies
  the login's record, the object alone, into the workspace's config, under `.claude.json.lock`.
- *The shared credential volume must be a local Docker volume*: becomes the credential volume (where
  the refresh lock now lives), and each per-workspace config volume too (its `.claude.json.lock` is a
  `mkdir` lock); the Feature's network-filesystem refusal covers both directories.
- *Never reap `.oauth_refresh.lock`*: its path is `$CLAUDE_SECURESTORAGE_CONFIG_DIR/.oauth_refresh.lock`
  (and in `shared`, still the volume's root).
- *Anything Drydock writes into the shared `.claude.json` takes Claude Code's own lock*: now each
  workspace's `.claude.json`, the seed included.
- *Pin the Claude Code version*: the bump re-runs Spike 09's `bump` mode as well as `00`–`03`.
- Design §7.1's struck mitigation (b) stays struck *without the variable* (measured: a lock per
  container), and gains a row for this layout.

**What the split changes about the threat model.** Spike 08 phase 1 found the shared volume carries
code-running inputs (hooks, plugins, skills, the user `CLAUDE.md`, MCP servers) from every workspace to
every other. Under `split` those are per workspace, and that path is gone; what stays shared is the
credential, which any workspace can still read, replace or blank (as today). The credential dir is
mounted read-write in every workspace, so a workspace can also plant files there beside the
credential — and whatever a later Claude Code version reads through the secure-storage resolver
(`HS()`) would then be read from a dir every workspace writes; today that is the credential and its
locks (result 6), and `./run.sh read` is where a new reader would show. Isolation is not a goal of
the adopted model (design §13), so this is a side effect, not a reason; but the residual-risk paragraph
of Spike 08 would no longer describe a `split` instance.

## Owed (human steps)

Written as steps in the [harness README](harness-09-split-credential/README.md#the-human-steps):

- **H1** — the owner's use case end to end: a turn from the phone answered (`APP1`), the same
  conversation joined from a terminal with `claude attach`, a turn typed there (`TERM2`) answered and
  shown on the phone as a turn of the same session (`WAIT_APP=300 ./run.sh app`).
- **H2** — a live refresh under split on a **disposable login the owner makes for the run**
  (`./h2.sh up`, `login`, `expire`, `start`, `record`, `down`): two daemons and a server on one
  credential volume, each with its own config; one refresh, the others adopting it, the credential not
  blanked, every session keeping Remote Control. Never on a copied credential. *Disposable* means
  made in H2's own login container and used for nothing else: best a separate account; otherwise a
  fresh `claude auth login` of the owner's account, which is an OAuth grant of its own that H2
  rotates and finally logs out. That rotating or revoking one grant leaves the account's other logins
  alone is an **assumption this spike has not verified**; if it is wrong, H2 signs the owner's other
  logins out too (recoverable by signing in again).

## Open questions

- Whether a daemon that announced `re-auth required` after losing the refresh race recovers by itself
  within its 30 s re-check once the winner's token is on disk (H2; it bears on `--bg` in either layout).
- Whether `claude auth status --json` takes the refresh lock with an expired access token: in `blank`
  a `.oauth_refresh.lock.owner` from a container that had run only `auth status` was found (on the fake
  credential, nothing blanked). The identity watch runs it every six hours.
- Whether the per-workspace config needs anything else a long-lived shared one had (a setting, a
  dismissed notice) to keep `claude attach` free of first-run screens; Spike 06's onboarding seed was
  enough here, and `attach` on a config without it was not tried.
