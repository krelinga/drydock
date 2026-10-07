# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

**Mostly design, with the first code in.** The repository contains the overall design
document (`docs/design/overall/drydock-design.md`, draft v39), supplemental ones on port forwarding
(`docs/design/port-forwarding/`, draft v6), testing (`docs/design/testing/`, draft v19) and the Vue
frontend (`docs/design/frontend/`, draft v19), a settled brand mark (`docs/design/brand/`, v1.1,
with the shipping icon assets), an adversarial security review
(`docs/design/security-review.md`), their SVG diagrams, a devcontainer definition, and **five
completed spikes** with their harnesses under `docs/design/spikes/` — the four Phase 0 ones plus
`04`, the browser-tier local CA, which the testing plan asked for later.

The Go module is `github.com/krelinga/drydock`. What exists so far: the **five testability seams**
from testing §5, the recorded fixture corpus, the five classifiers built on it, and the server side of
Phase 1 — a `drydock` binary that serves the gated API on two Unix sockets behind a tested Caddyfile — and Phase 2's walking skeleton, `POST /api/workspaces` to a running container:

```sh
go build ./... && go vet ./... && go test ./...   # the whole suite; the Caddy test needs `caddy` on PATH
gofmt -l .                                        # must print nothing
cd web && npm ci && npm run check                 # the UI: types, tests, dist is current, size budget
test/install/run.sh                               # the installer, in a systemd container
test/browser/run.sh                               # the browser tier: Chromium (+ Firefox, WebKit), Caddy, drydock (needs web/'s npm ci)
go build -o drydock ./cmd/drydock                 # the one binary
printf "%s\n" "$PW" | ./drydock passwd --db x.db  # set the operator password (no HTTP route can)
./drydock serve --db x.db --ui-origin https://drydock.example.com --ui-host drydock.example.com \
  --socket-group drydock                          # see `./drydock serve -h` for the rest
```

| Package | Is |
|---|---|
| `internal/api` | The route table as **data**, both muxes, the gate interface, the error envelope — and ten meta-tests that walk the table. `PUT /api/secrets/:name` is one route with two meanings: create-or-replace, or with `If-None-Match: *` a create that refuses a stored name `412 secret_exists` (any other `If-None-Match` is `bad_request`). A route refused because Drydock is shutting down answers `503 unavailable`, never `internal`. |
| `internal/sys` | `Clock`, `DiskUsage`, `Random`. Never call `time.Now()` directly. |
| `internal/subproc` | An invocation described as data, resolved by `PATH` or an injected `Resolver`, and `Exec`, the real runner: no shell anywhere, `Env` replaces rather than inherits, a non-zero exit is data, and cancelling sends `SIGTERM`. `StartPTY` is the same rules on a terminal (`internal/pty` composed, not reimplemented): the supervisor's and the login handshake's way to start a child that must see a PTY. |
| `internal/config` | Settings that must not be constants, `LabelPrefix` chief among them, plus a `Validate` that refuses configurations which silently undo a design property — among them a UI origin, host or preview domain that is not lowercase, since a browser sends the `Origin` host lowercased and that check is exact, and a preview domain on the UI host's registrable domain (`CrossSite`: eTLD+1 by `golang.org/x/net/publicsuffix`, the list browsers decide `SameSite` by, so a parent or sibling is refused as a subdomain is; a name that is itself a public suffix is refused rather than guessed at). The installer asks the same function through `drydock check-preview-domain`. The shared Claude volume's name (`ClaudeVolume`, default `drydock-claude-config`), the Claude image's digest-pinned base, and the identity watch's interval and expiring window live here too. |
| `internal/store` | SQLite in WAL mode, the single-instance lock, and §4's schema with its enumerations as `CHECK` constraints. A golden snapshot pins the schema. |
| `internal/classify` | The five classifiers, implemented and tested against the corpus: login, identity, refusal, discovery, container. Built in parallel by four agents, one file each. |
| `internal/claudetest`, `internal/pty` | `fakeclaude` (testing §6.4): a stand-in `claude` that replays the corpus onto a real PTY of a chosen width, scripted by a JSON file beside the binary (not an env var, which `subproc` replaces), and logs what it received — each code as a SHA-256 with its framing judged, anything typed at a gate or a serving server, a missing PTY. Every file it replays is pinned by SHA-256 in `claudetest.Pinned`, so a re-record stops it until it is re-derived. `Install` builds and scripts it; `Start` runs it on a PTY; `Events`/`NoViolations` read its log. `internal/pty` is the PTY itself, on `x/sys/unix`, usable by the supervisor too. |
| `internal/auth` | argon2id with a floor and rehash-on-sign-in, sessions stored only as SHA-256, and a lockout that is per-IP backoff plus a global cap, kept in `auth_attempt` so a restart does not reset it. |
| `internal/events` | The append-only event log and its live fan-out. Append writes and publishes under one lock so subscribers see id order; a subscriber that lags 256 events is cut off rather than allowed to block writers, and recovers by replay. A subscription's channel is closed in exactly one place, behind one `sync.Once`, so `Cancel`, `Close` and a lag cut-off are safe in any order and any number of times — and `Subscribe` after `Close` returns one already closed, since a goroutine `Serve` started can subscribe after shutdown closed the log (a second close was v0.4.0's release-run panic). `data` is a JSON object for the reducer; `message` is prose nothing may parse. `Commit(ctx, fn)` runs `fn`'s transaction and appends the events it returns in that same transaction, under the same lock, publishing only after the commit: a row change and its event are one fact, so commit order is id order is publish order for every writer. |
| `internal/workspace` | The workspace state machine — a transition table where `deleting` is a sink and nothing reaches `running` except from a build — and design §6's eight steps, each writing `workspace.step` started/done/failed so a failure names its step. A step's raw error never reaches an event (a subprocess's stderr can carry anything, git's quoting the URL); only a `workspace.Public` sentence does. Creates check the duplicate and the cap inside one transaction, which is why the store opens every transaction `IMMEDIATE`. `Remove` (only from `deleting`) takes the `supervisor` row with it — and its repository's row and secret grants, when the installation had dropped the repository and this workspace was what held it — and keeps the event log, `token_grant` and `secret_access` — the "which workspaces ever held this secret?" history outlives the workspace. A repository row it releases is announced in the same commit as `repo.removed` (`store.KindRepositoriesRemoved`, `{repository_ids}`), after `workspace.gone`: a `repo.*` event is what makes an open catalog refetch. `Annotate` sets `state_detail` without a move (a stuck delete, a failed stop) and `ClearDetail` removes it as the retry starts, each a `workspace.state` event with `from` equal to `state`. The cap's rule is `Occupying` and nothing else: the SQL lists in `Create` and `Occupied` are built from it, and `CapacityOf` counts the very rows `GET /api/workspaces` lists by it. The view's `last_action` is the newest `workspace.action` event, so the UI reads which sub-step a failed stop stopped at as structure, never from the sentence. `Create`, `Move`, `Annotate`, `ClearDetail`, `Remove` and `Adopt` write their row and their event through one `events.Commit`, so two movers cannot publish out of commit order (a delete overtaking a run's move to `running` once left the stream on `running` while the row said `deleting`; a stress test runs that race). The views read only live workspaces' step, action, supervisor and session events, the newest per step and per kind, in SQL; an event whose data cannot be read is skipped and logged (`Logf`), never a `500` for the whole list. `approval.go` is the host-access gate's state (design §6): a step returning `NeedsApproval` ends `needs_approval` (never `failed`) and the workspace moves `building` → `stopped` with the request in `pending_approval` and on that one `workspace.state` event (`data.approval`, and the view's `approval`); any `Move` clears it. `Approve` supersedes the repository's current `config_approval` row and inserts the new one in one `events.Commit` with `config.approved`; `Decline` clears the request with a same-state event. |
| `internal/clone` | Design §6 step 2, the host clone. A `contents:read` token for the one repository reaches git only through its environment: a `GIT_CONFIG_COUNT` credential helper that prints it from an environment variable. It is never in argv, a URL or `.git/config`. git runs with global and system config at `/dev/null` and `GIT_CEILING_DIRECTORIES` above the workspace, so an enclosing repo's `http.extraheader` cannot win. It never deletes a directory already at the clone path. Tested with a `GIT_TRACE` wrapper that records every child process's argv, plus a canary sweep of the tree and the database. |
| `internal/container` | `devcontainer up` (argv built and validated from workspace data, result read by `classify.ClassifyContainer`) and finding containers by label: `docker ps -q --filter label=…` for ids, `docker inspect` for structured labels and state. Never a table parse. The id-labels carry workspace id, repository id, repo and branch, so a row can be rebuilt from them. `read-configuration` is parsed too (one JSON object; an unparseable `devcontainer.json` exits `0`, so "names no image" is checked here), run with `--include-merged-configuration` and an id-label no container carries, so the merged configuration is computed from what `up` will read, never from an old container's labels. `HostAccessOf` (`policy.go`) is the **host-access subset**: every field outside an allowlist, from the configuration and the merged one (a Feature's `privileged` is its own entry, source `feature_or_image`), as `{field, source, value}` with canonical JSON values and the clone's path written `${localWorkspaceFolder}`, hashed by `HashSettings` (`sha256:` over the sorted entries) — `initializeCommand`, `runArgs`, `appPort`, `workspaceMount`, Compose, `build.options`, a bind mount or any volume not named with `${devcontainerId}`, `privileged`, `hostRequirements.gpu`, `capAdd`/`securityOpt` beyond the `SYS_PTRACE`/`seccomp=unconfined` debugger pair, a Dockerfile or build context outside the clone, and **any field it does not name**. A config file that is a symlink or outside the clone is `ErrConfigFileOutside`, not a setting. `DiffSettings` is what the operator is shown, and `Covered` decides: every current entry within the approved set — the same canonical value, or for `capAdd`/`securityOpt`/`mounts` a list of approved elements; `runArgs` must equal, since its elements are argv and recombine. The hash is only the approval request's staleness check. A repository's committed `devcontainer-lock.json` is **honoured** (`LockfileHonour`: no lockfile flag) and one without gets `--no-lockfile`, so none is created — never `--frozen-lockfile`, which refuses a lockfile a commit stale that VS Code would quietly rewrite. With no flag `up` leaves an in-sync lockfile byte for byte and rewrites a stale one to VS Code's bytes, **but only because Drydock's Feature has no `dependsOn`**: the CLI writes an injected Feature's dependencies into the lockfile (design §6, "The repository's lockfile"; the measured matrix, with Drydock's real Feature across create, start and rebuild, is `test/fixtures/devcontainer/lockfile-behaviour.txt`). A lockfile with an entry for Drydock's own Feature, or one that is not a regular file inside the clone or does not parse, is refused. Each `up` gets its own `TMPDIR`: the CLI stages Features in a folder named by the millisecond, and concurrent creates shared one. `--override-config` goes to `exec` as well as `up` — `exec` reads the config too. The broker is mounted as the workspace's **directory**, `UpSpec.BrokerDir` at `BrokerMountPoint` (`/run/drydock`), so the socket is at `BrokerSocketInContainer`, `/run/drydock/broker.sock` — the path the Feature always used, so the Feature did not change. Not read-only: the CLI's `--mount` regex takes `type`, `source`, `target` and `external` and nothing else (CLI 0.89.0 refuses `,readonly`). `LegacyBrokerMount` (and `Found.LegacyBrokerMount` in `List`) reads `docker inspect`'s `Mounts` for a bind at `LegacyBrokerMountPoint` — the socket file an earlier Drydock mounted, which only a rebuild replaces. `Find`/`Stop`/`Remove` act on the containers carrying one workspace's label (never a cached id); every id handed to `docker stop`/`rm` must be a full 64-hex id, after `--`; `rm` is `--force --volumes`, which takes anonymous volumes and never named ones. `RemoveContents` is the delete's **cleanup helper** (`cleanup.go`): `docker run --rm` of `CleanupImage` (configuration, busybox **pinned by digest** — `Validate` refuses a tag) as root with `--network none`, `--read-only`, every capability dropped but `DAC_OVERRIDE` and `FOWNER`, and one bind mount — `<root>/<id>` at `/w`, never a parent — running `find /w -mindepth 1 -delete`. It carries `<prefix>.cleanup=<id>`, never `<prefix>.workspace`, so reconciliation's listing never sees one; a stray left by an earlier attempt is removed by label before the next runs. Everything before the helper's own `docker run` that fails is `ErrCleanupNotRun` (an unpinned image also `ErrCleanupImage`), so the delete never says a helper was tried when none ran. `ListHelpers` is the boot sweep's listing: the bare cleanup label, and nothing that also carries the workspace label. `BuiltImages(folder)` are the names `up` gives the images it builds, measured on CLI 0.89.0: `vsc-<basename>-<sha256 of --workspace-folder>` and that with `-features`, `-uid`, `-features-uid` (nothing labels them, so the exact name is the handle); `RemoveBuiltImages` lists those by exact reference and `docker image rm`s them — never `--force`, never a prune, so Docker itself refuses an image a container still uses. `EnsureClaudeVolume` (§6 step 4, and the login's first act, so the two never disagree) makes the shared credential volume local and labelled, refuses a foreign or non-local one, and then runs the **owner helper** (`volumeowner.go`: the pinned busybox as root with only `CHOWN`, `FOWNER` and `DAC_OVERRIDE`, `--network none`, the volume alone, label `<prefix>.volume-owner`): an *empty* volume, whoever owns it, is given to `ClaudeUID` (Drydock's own) `0700` with a marker directory `.drydock-volume` left in it, because Docker copies an image directory's owner into a volume whenever it is mounted while empty — and the Feature's `/home/vscode/.claude` carries a non-`vscode` remote user's *build-time* uid, which gave a fresh volume to the wrong uid (measured in `test/container`). A written volume another uid owns is `ErrVolumeOwner` naming it. The CLI's `--mount` cannot say `volume-nocopy`. `feature/prepare-test-volumes.sh` carries the same line for the Feature's tests, compared by a Go test. |
| `internal/provision` | Design §6 end to end, behind `POST /api/workspaces` and `/start`: the eight real step functions, run off the request path, one run per workspace, bounded by a timeout and by shutdown (each fails the interrupted step with a sentence saying which). An honoured lockfile is never repaired: after `up` its bytes are compared with what was there before (bytes, not `git status` — git on the host would run whatever the container-writable `.git/config` names), and a stale one `up` rewrote stays in the clone with the `up` step's detail naming the file, since it is exactly what VS Code would write and the right thing to commit. A repository with no `devcontainer.json` — decided by looking for the file, since `read-configuration` fails silently — gets `mcr.microsoft.com/devcontainers/base:debian` written *beside* the clone. Step 3 stops the workspace's containers, then computes the host-access subset and, unless it is within the repository's current approved set (`container.Covered`), returns `workspace.NeedsApproval` before any `up` — on create, start and rebuild alike; the stop is what makes the file checked the file `up` reads. `ApproveConfig` (the hash shown, else `ErrApprovalStale`) records it and continues the stopped run with its own `--remove-existing-container` under the cap; `DeclineConfig` drops the request. Drydock's own override config is checked without the clone path checks. Step 4 makes the shared credential volume (`config.ClaudeVolume`, default `drydock-claude-config`) if an exact-name listing lacks it — local driver, `<prefix>.claude-config` label — and refuses one without that label or with a non-local driver or driver options (NFS/CIFS through the local driver); `up` mounts it at `container.ClaudeConfigMountPoint`, the Feature's `CLAUDE_CONFIG_DIR`. Step 7 also runs `claude --version` and requires `classify.ClaudeCodeVersion`. Step 8 hands the running workspace to the supervisor (`StartSupervisor`) and does not wait for serving; `SessionSpec` is how the supervisor execs in (the override decided as step 3 decides it, the remote env passed again, the session-name prefix the repository's short name). `RestartSupervisor` (POST …/supervisor) is a job under the same ownership, and `ResumeSupervisors` is boot adoption's half. **A container an earlier Drydock made, with the broker socket mounted as a file** (`container.LegacyBrokerMount`), is named, never left to exit 69: `ResumeSupervisors` calls `ParkSupervisor` instead of starting it (the server wires `supervisor.Park` with `stale_broker_mount`, which the card reads as *Container misconfigured* + Rebuild), and a start — not a rebuild — fails at `up` with `LegacyMountSentence` before running it. Up mounts `Broker.SocketDir`; a stop `Close`s the socket and keeps the directory; a delete's `broker_socket` sub-step is `Broker.Remove`, and its `ErrLeftover` is a note, not a stuck delete.Note`) until the supervisor. `Unowned(id, act)` is reconciliation's `Exclusive`: it runs `act` under the lock every job starts under, only if this process has started no job for the workspace, so a create in the first seconds after boot is not marked "interrupted" and a stop or start asked during boot is never acted against by a plan made before it (a check and then the act left that window). `ReopenSockets` and `ResumeSupervisors`, boot's follow-ups, likewise decide from the row read again under the lock. A delete's `containers` sub-step also removes the workspace's `BuiltImages`, as a note rather than a stuck delete when Docker refuses. One workspace per repository in any state; a start honours the cap. Tested with a fake CLI for argv and every step's failure sentence, and in `test/container` for real. Phase 6's `Stop`, `Rebuild` and `Delete` (`lifecycle.go`) are jobs under the same one-per-workspace ownership, each sub-step writing `workspace.action`: a stop is refused unless `running` and closes the broker socket; a rebuild (and a start from `failed`) passes `--remove-existing-container`; a delete is persisted as `deleting` first, cancels and waits for a run in flight, and removes only `<WorkspaceRoot>/<ULID>` (clone, `.drydock/` and its per-`up` `tmp/`) proved to be a real directory matching the row (never following a symlink). What the host cannot remove — root-owned files a process in the container left in the clone — goes through the container package's cleanup helper, after the directory is proved a second time, and the `files` sub-step's detail says the helper ran. A run that ends `failed` closes its broker socket (§9.1: access follows Drydock's state; a failed postCreateCommand's container is still up, but not handed over), and start or rebuild reopens it at step 5. `StopSupervisor` is the supervisor's `Stop`, run *before* the container in a stop, a rebuild and a delete. `ResumeDelete` is reconciliation's. Crash-tested by cutting a delete off after every sub-step and resuming in a fresh process. A failed stop annotates the still-running row (`StopFailedDetail`), so a list snapshot says it as the live events did; a stop asked again, and a resumed delete, clear their annotation under the lock before the job's first event. `SweepHelpers` is the boot sweep: after reconciliation, cleanup helpers by this instance's label, skipping any workspace with a job in flight, under the lock. |
| `internal/supervisor` | Design §8: one `claude remote-control` per running workspace, through `devcontainer exec` on a PTY Drydock owns (`container.SessionArgs`: `sh -c` of the constant `RemoteControlLaunch`, which records the pid in the container, runs the secrets prelude and execs the server). **Signalling `devcontainer exec` does not reach the server** (measured) — so every stop is `container.SignalSession`: `docker exec -u 0` of a constant script that signals the recorded pid only if it is a `remote-control`, SIGTERM, SIGKILL after `StopTimeout`. Every start first stops any server the pid file names, which is how boot adoption replaces the server an earlier Drydock left serving; `Detach` (shutdown) closes terminals and signals nobody. Exits are classified by `classify.ClassifyRefusal`, never the status: the registration wait is `waiting_registration` on a flat retry and never charged to the budget (2 s→60 s, 6 in 10 min, then `degraded`); the organization refusal is `awaiting_login`; trust and `--spawn` are `degraded`, not retried. No environment within `GateTimeout` is a hang: stopped, then named by its prompt, never answered. A stored identity of blanked or absent starts nothing and spends nothing; `Watch` resumes on `auth.identity`. **`expired` starts the server**: it dates the access token, which the server renews from the live refresh token (Spike 00), and parking every server on it left nothing to refresh (a dead refresh token is blanked, not expired). Discovery is `classify.ClassifyDiscovery` over a 16 KB raw window (transient): environment id to `workspace.environment_id`, OSC 8 session ids to `rc_session`, `Capacity: N/M` to `session.status`. The log is a 1 MB `Ring` of redacted visible lines (token shapes and the workspace's granted secret values), served by `GET …/logs`, never persisted or mirrored to `event`. **The ring owns its redaction**: `NewRing` takes the values' source (`Manager.Redact`), asked on every `Write`, `Flush` and `Mark`, and `add` — the only way in — masks, so no call site passes a list and none can pass `nil` (two `Flush` calls once did, and a final unterminated line carried a secret's value out unmasked). It masks every value it was ever given, longest first, so a rotated-out or revoked value, or a moment of undeliverable snapshot, unmasks nothing. The server's source caches each workspace's repository id and reads values from the broker's snapshot, so no query per read and never a stale set. Every reason is a code (`Reason`), the UI's key. `Park` records `degraded` with a reason Drydock found before starting (`stale_broker_mount`), starts nothing, holds nothing in memory, and leaves alone a server already running there; only an explicit `Start` (a rebuild's step 8) starts it. Tested against `fakeclaude` behind fake `devcontainer`/`docker` binaries that run the real launch line and signal script on the host, and in `test/container` through the real CLI and Docker. |
| `internal/reconcile` | Boot reconciliation (§6) as a pure `Plan(rows, found)` plus `Run`. Adopt rather than kill, never auto-start, and a failed `docker` listing changes nothing. Wired into `Serve` beside serving, so each action is decided and applied inside `Exclusive` (`provision.Unowned`), one call under the job lock; a resumed delete, which takes that lock itself, is only checked there. Without Docker access it writes one warning and carries on. A `deleting` row is finished by `provision.ResumeDelete` — the route's own delete — and broker sockets are reopened after it, for `running` workspaces only. Then `SweepHelpers` removes any cleanup helper an interrupted delete left (tested in `test/container` beside a workspace container, one carrying both labels, and another prefix's helper, none of which it touches). `Run`'s error wraps `ErrNothingChanged` only when it could not read Docker or the rows; a run in which some actions failed is a `*Partial`, and the boot warning then says how many workspaces could not be reconciled rather than that nothing changed. |
| `internal/github` | The App client: the key loaded from a file (refused if its group or others can read it), an RS256 JWT built on the standard library, installation tokens cached in memory for 55 minutes by what they grant, and paginated listing. A `Token` formats as `[redacted]` and refuses to marshal, and a token request must name its permissions. `githubtest` is the fake GitHub: it verifies the JWT and enforces each token's permissions and repositories, and `EnableGit` adds a git smart-HTTP remote (git's own `http-backend` behind GitHub's token authorization) that records every token it is shown. |
| `internal/catalog` | The repository list: refreshed at boot, every 15 minutes and on demand, with concurrent refreshes joined. A listing token asks for metadata only and a probe token for contents read. A repo is probed for `devcontainer.json` only when it was pushed to, and a failed probe is `null`, never `false`. A repo dropped from the installation is deleted unless a workspace holds it, and its secret grants with it. One a workspace holds keeps its row and grants while that workspace lives (§12: the broker keeps serving it what it had) and loses both when it is released: `store.DropReleasedRepositories` runs in `workspace.Remove`'s transaction and on every refresh (which sweeps rows an older release left), so a re-added repository is granted nothing. Deleting grants outside `internal/secrets` must call `secrets.Store.Invalidate` (the `GrantsDropped` hooks on `Catalog` and `workspace.Store`), or the broker serves them from its snapshot to the re-added repository. `token_grant` and `secret_access` are never touched. |
| `internal/identity` | The expiry watch (§7.3): at boot, every six hours and on `POST /api/auth/claude/check`, it reads the shared Claude volume and stores `classify`'s verdict in `claude_identity`, announcing changes as `auth.identity`. The volume is read through short-lived containers — read-only mount, `--network none`, only `DAC_READ_SEARCH` — the file with the pinned busybox (so blanked and absent never wait on anything else) and `claude auth status --json` in the Claude image; a missing volume is absent without a container, which would create it, and a volume without `<prefix>.claude-config` (`identity.LabelVolume`, which is `container.LabelClaudeConfig`, the label §6 step 4 gives the one it makes) is a failed check, `foreign_volume`, and never read. A check it cannot read keeps the stored state, updates `last_checked_at`, and emits `auth.identity_check_failed` in Drydock's words. The credential's bytes reach nothing but the classifier: a canary sweep covers the DB, events, the log, the HTTP body and every error. `LoggedIn(at)` is the login handshake's: it waits out a running check, checks afresh, and the first live verdict records `at` as `logged_in_at`; the moment is consumed by the next stored verdict whatever it is, so a handshake over a volume still showing no login dates nothing later. **Every check ends** (#37's review: a workspace can put a FIFO at the credential path, and the read hung the watch for good): the reader's constant line refuses a symlink or anything not a regular file before opening it and reads with `head -c` one byte past the 64 KiB cap; each read is bounded by `Timeout` (`--identity-check-timeout`, two minutes) and the image's first build by `BuildTimeout`, both through `sys.WithTimeout` on the injected clock; a read cut off is a failed check, problem `timeout`, keeping the stored state; after any failed read, and in the first check after boot, helpers carrying `<prefix>.identity` are removed by label (`Sweep`) — a killed `docker run` client leaves its container running, measured. Checks run one at a time and sweep inside the one running. `Trigger` runs under the watch's own context, which `Shutdown` ends and waits for before the database closes. **`expired` is a live login** (§2.4): `expiresAt` dates the access token, which a starting server renews from the refresh token beside it; its event is `info` and the UI shows no fault for it. |
| `internal/claudeimage` | The one image Drydock runs Claude Code in itself (the watch and the login handshake), built locally from a digest-pinned base and `@anthropic-ai/claude-code` at exactly `classify.ClaudeCodeVersion`; the build fails unless the binary reports that version, the tag hashes the recipe, and the image is run by ID. |
| `internal/login` | The login handshake (§7.2). `Manager` runs one login at a time (a second is `ErrInProgress`): `Begin` announces `starting` before the route's `202`, then a `Launcher` starts `claude auth login --claudeai` on a PTY Drydock owns, started through `subproc.PTYRunner.StartPTY` — the supervisor's one PTY start, so there is no second mechanism, and every phase change is `auth.login` carrying the whole `View` (`login_id`, `phase`, `url`, `deadline`, `attempts`, `problem`, `message` — no field for a code). Every verdict is `classify.ClassifyLogin`'s: a validated URL and the prompt are `awaiting_code`; after a code, the verdict for *that* code is read from the stream up to the prompt plus what arrived since, so an earlier `Invalid code` (the prompt is never re-printed) cannot answer a later one — tests catch that only with fakeclaude's replay paced. `Submit` checks `classify.ValidateCodeShape` first — on the bytes, which it never copies into a string no one could zero (a test measures zero allocations) — types the trimmed code and one `\r`, and zeroes its copy; the PTY buffer is memory only and zeroed at the end. Five minutes from the URL on the injected clock is `timed_out`; a ten-minute start timeout covers the image's first build. **Every end kills the process and removes the container by label before it is announced**; a success is announced at once, then `IdentityRecorder.LoggedIn(at)` (the watch), under the manager's own context, which `Shutdown` ends. `DockerLauncher` is production: `EnsureClaudeVolume` first — §6 step 4's own call, which makes the volume labelled (a `docker run` would create it unlabelled), gives an empty one to Drydock's uid and refuses one another uid has written to (`ProblemVolumeOwner`, both uids named) — then `docker run --rm -it` of the Claude image as **Drydock's own uid** (the uid every workspace's remote user gets; the credential is `0600`), the volume read-write at `/home/vscode/.claude`, `--cap-drop ALL`, read-only root, label `<prefix>.login=<id>` — never `.workspace` or `.identity`. A killed docker CLI leaves its container running (measured), hence removal by label and `Sweep` at boot, which a launch waits for. One killed during its create leaves a container the daemon finishes creating after the CLI is gone, so `Remove` is told whether Drydock killed the CLI (`killed`, false once the PTY has ended by itself) and then keeps listing for up to `RemoveSettle` (3 s), and every launch first sweeps all other login containers. `logintest.Launcher` runs fakeclaude on a PTY directly (through the same `StartPTY`) for the component tier. A success reaches the supervisors only through the watch's `auth.identity`, which they resume on: `internal/supervisor/login_test.go` drives both halves with fakeclaude, a wrong code as the control. Tested there (with the canary sweep, and again through the real server's socket, HTTP responses and SSE transcript included), in `test/container` with fakeclaude in a real container — where the PTY semantics Spike 01 relied on are measured through `docker run -t` — and in the browser tier. |
| `internal/broker` | The token broker (§9). Each workspace gets a socket, `<dir>/<id>/broker.sock` (`0666`, in a `0755` directory of its own inside the `0700` `<dir>`), and the socket a connection arrives on decides the workspace. **The workspace's directory, not the socket, is what its container mounts** (`SocketDir`, at `/run/drydock`): a bind mount pins an inode, and a mounted socket *file* died with the process that bound it, so a Drydock restart left every running container with a dead broker — git, `gh` and every command's prelude exiting 69 (#16's review). So the directory must outlive the process: `Open` never recreates one that exists, `Close` (and `CloseAll` at shutdown) removes only the socket, only `Remove` (a delete's) takes the directory, and the unit sets `RuntimeDirectoryPreserve=yes`. The CLI cannot mount it read-only, so root in the container can write there: `Open` binds and chmods at a staging name in `<dir>` and renames into place, replacing whatever is at the name without following it (a directory there is refused); `Remove` is `RemoveAll`, and what it cannot remove is `ErrLeftover`, which the delete notes rather than sticks on. `Open` refuses a socket path over sun_path's 107 bytes, which binding the shorter staging name would not catch — so a test's broker dir comes from `os.MkdirTemp("", …)`, never `t.TempDir()`. The line protocol is `GET-TOKEN scope=git` or `scope=gh`, plus `PING`, parsed strictly: an extra field is `bad_request`. Each scope asks for exactly §9.3's permissions (golden files in `testdata/`), for exactly the workspace's one repository. The repository's state is read per request, a GitHub refusal never falls back to anything broader, and `token_grant` and `token.issued` are written per mint, not per cache hit — the row **before** the token is first served: one whose row cannot be written is `unavailable` and is not marked recorded, so the next request retries the write on the same cached token (as `GET-SECRETS` will not answer unrecorded); either failure goes to the journal through `Logf`, never with the token or a value. A mint's refusal is a reason from a closed set, and **a `422` is two refusals only GitHub's message tells apart**: *permissions requested are not granted* (the App lacks one, or the installation has not accepted it) is `app_permission_missing`, *not accessible to the parent installation* is `revoked`, and any other `422` is `unavailable`, never a guess. The matchers are `github.IsPermissionNotGranted` and `IsRepositoryNotIncluded`, pinned against the real dev App by `TestContractTokenRequestsBeyondTheAppAreRefused` (which asks for `administration: write`), and through the broker against the fake by `TestAMissingAppPermissionIsNotARevocation`. The broker's `TestContractGHScopeMints` mints the `gh` scope live from each testbed's socket and checks the token lists that repository alone. `token.refused` carries the scope and its permission set and a fixed sentence per reason — never GitHub's text. `GET-SECRETS` (no arguments, ever) answers `OK count=N`, N `NAME value` lines, `END` from the secrets store's snapshot — no GitHub request, no decryption — and writes `secret_access` before answering, or does not answer. `Close` also removes a stale socket file an earlier process left — and one at the pre-directory path `<dir>/<id>.sock` — so a delete resumed at boot leaves no socket. |
| `internal/secrets` | Repository secrets (§10). XChaCha20-Poly1305 under a 32-byte master key read once from a `0400` file (`Key` formats as `[redacted]`), a fresh nonce per write, the secret's id as AAD. Validation on write: the name pattern, the reserved list (exact names plus the `CLAUDE_`, `ANTHROPIC_`, `DISABLE_`, `GH_`, `GIT_`, `DRYDOCK_`, `LD_`, `BASH_` prefixes — each with its reason, which the refusal quotes), any Unicode control character or non-UTF-8 in a value, an empty or over-32-KiB value, a blank reach (`secret_reach_required`) apart from a long one (`secret_reach_too_long`), a long description. Default deny; `all_repos` widens to every repository. `Resolve` serves the broker from a snapshot decrypted once per write, and refuses *everything* if any row is undeliverable. That condition is **state, not just an event**: `Undeliverable` names every broken row and why, `GET /api/secrets` reports it, and its changes are `secret.undeliverable` and `secret.deliverable`. A write rebuilds the snapshot at once, so a repair is announced within its request, and boot checks too. `Put` answers the stale running workspaces split by `StaleKind` (a hook; nil means `new_commands`, true until Phase 5). `PutProse` keeps the stored value and is never a rotation; it is the `PUT` with **no** `value` key, which is a different request from `"value":""` (refused) and `null` (`bad_request`), decoded explicitly by `optionalValue` in `internal/api`. `Meta` has no field for a value. `Create` is `Put` that refuses a stored name with `ErrExists` inside the write's transaction, nothing written: the API's `If-None-Match: *`, which the UI's New secret always sends. |
| `feature/src/drydock/` | The devcontainer Feature `drydock` (§11), published to `ghcr.io/krelinga/drydock/drydock` by `feature-publish.yml` when its own version changes. It installs the clients into `/usr/local/drydock/bin`, linked from `/usr/local/bin` because a login shell drops `containerEnv`'s `PATH`. It installs `gh` itself (GitHub's apt and dnf repositories, Alpine's `github-cli`; an image's own `gh` is kept; any other image fails the install) and **must never declare `dependsOn`**: the CLI writes an injected Feature's dependencies into every repository's `devcontainer-lock.json` (design §6, §11). It sets system git config (helper, bot identity, `core.hooksPath`) and adds a `pre-push` guard for the `drydock/` prefix that passes every other hook through to the repo's own. `containerEnv` points `CLAUDE_ENV_FILE` at `etc/claude-env.sh`, a shipped file whose whole text is `eval "$(drydock-secrets export || echo exit 69)"`; nothing writes it at install or start. **The Claude half (Feature `1.0.1`, major `:1`):** Claude Code at exactly the `claudeCodeVersion` option (default `2.1.289`; Drydock passes `classify.ClaudeCodeVersion`), downloaded from `downloads.claude.ai/claude-code-releases` and checked against SHA-256s pinned in `install.sh` — never `claude.ai/install.sh`, which runs the *latest* binary to install; `containerEnv` sets `CLAUDE_CONFIG_DIR=/home/vscode/.claude` and `DISABLE_AUTOUPDATER=1`. `postCreateCommand` is `drydock-preflight` (refuses, naming each, §2.1's five variables in the environment or a settings `env` block, a moved `CLAUDE_CONFIG_DIR`, a missing/extra/nested/network mount there, a root remote user, one that cannot reach the directory, or one that does not own it — the volume is Drydock's uid, given by step 4, so that means a remote user the CLI did not move to Drydock's uid, and the sentence says so) then `drydock-claude-config` (merges `remoteDialogSeen` and the workspace folder's `hasTrustDialogAccepted` into `.claude.json` **under Claude Code's own `.claude.json.lock`**, a mkdir lock it never steals; never writes `oauthAccount`). Major bumped because it refuses a container without the shared volume an older Drydock never mounts. **Bump `version` in `devcontainer-feature.json` with any change here**, or it is not republished. Tested by `devcontainer features test -p feature --skip-autogenerated` in CI's `feature` job, against a stand-in broker, after `feature/prepare-test-volumes.sh` prepares each scenario's volume as step 4 would (so `ubuntu-bare`, a non-`vscode` remote user with the uid update on, is exercised), and by `feature/expect-fail/run.sh` — real builds that must fail, by name, beside a baseline that must come up, because the harness cannot express a scenario that should fail. |
| `feature/src/drydock/bin/` | The in-container clients, POSIX `sh`: `drydock-broker` (the transport, over `socat` or `nc -U`), `drydock-credential` (git's helper, for GitHub's host only) the `gh` shim (fetches a token per call and execs the real `gh` with it in the environment, not argv), and `drydock-secrets export` (single-quotes every value, `'` as `'\''`; fails the fetch unless `count=`, every line's shape and `END` agree; silent on success; on failure one stderr line **and `exit 69` on stdout**, because `eval "$(…)"` discards the helper's own status). `DRYDOCK_BROKER_SOCK` is `/run/drydock/broker.sock`; since Drydock mounts the workspace's directory at `/run/drydock` rather than the socket file at that path, it names the same place, which is why that fix needed no Feature change. `drydock-broker` turns `app_permission_missing` into a sentence that says what to check, and prints any reason it does not know as it came, still exit `69`, so an older Feature fails cleanly against a newer server. They are tested from Go against a real broker socket over both transports, and with real git against the fake's git remote. |
| `internal/server` | Assembles the front door: store, auth, both muxes, two `0660` group-owned sockets, and no TCP listener — asserted on the running process. `Serve` cancels its own context as serving ends, so a failed listener stops the loops it started (and returns) as a cancelled context does. The preview server has no `SecurityHeaders` and no handlers yet (`previewHandlers`); `TestPreviewSessionSendsNoReferrer` fails the day a `preview.session` handler appears without `Referrer-Policy: no-referrer` (security review F4). |
| `cmd/drydock` | `serve`, `passwd`, `count-secrets`, `check-preview-domain` and `version`, and nothing that binds TCP or sets a password over HTTP. `count-secrets --db` prints how many secrets are stored, for the installer's key-replacement guard: through `store.OpenReadOnly` (SQLite `mode=ro`), so no lock, no migration — the server it asks about may be an older binary — and no database created; a missing file or a schema without the `secret` table is `0`, anything unreadable an error, never `0` — and so is an **empty file**, which Drydock never leaves (it migrates on open): a botched restore's 0-byte database is not "no secrets", since the real one may come back. It writes nothing to the database, but reading a cleanly closed WAL database makes SQLite create its `-shm` and an empty `-wal`, which is why the installer asks as `drydock`, never as root. `passwd` deliberately skips the instance lock so it works while the server runs; `--if-unset` makes it a no-op that never reads stdin once a password exists, which is what keeps an installer re-run from signing everyone out. `check-preview-domain --ui-host H --preview-domain D` exits `0` only when `config.CrossSite` passes, `1` naming why, `2` on a malformed call. `version` is stamped by `-ldflags -X main.version=`. `serve --secrets-key` takes the master key's *path*. |
| `web/src/stores` | `reducer.ts` is the one pure writer of entity state. It applies events, `resync`, and three snapshots — `GET /api/repos`, `GET /api/workspaces`, `GET /api/workspaces/:id` — each tagged with the stream position it was requested at. Each field carries the id of the event that last wrote it, so a replay changes nothing and a late event is a no-op. **Only the workspace list may drop a workspace**: the catalog joins just each repo's newest, so its silence proves nothing. Each workspace has a step timeline versioned per step, and a feed of its last 50 events merged by id. `workspaces.ts` holds the fetches and the two Phase 2 mutations; a clone is settled by the create's `workspace.state` carrying its `repository_id`, because the only workspace id before that is in the `202` body, which is never read. `stream.ts` owns the `EventSource`, the in-flight set and the refetch hooks, and handles reconnects: a quiet marker after 5 s, and on `CLOSED` a session probe, then sign-out or a hard retry that resumes with `?last_event_id=`. Specs drive `test/fakeEventSource.ts`, because jsdom has no `EventSource`. **An in-flight mark ends two ways, by one predicate**: each action's end is `OVER.*` over an `Outcome` (gone, state, stuck delete, failed stop), asked of each event (`settles*`) and, after every snapshot (`stream.snapshot`), of the entities — but only for a mark whose request was accepted (its `2xx`) before that snapshot was requested (`snapshotTag`), since only such a body can tell *not begun* from *over*. That is what ends a mark whose settling event fell in a `resync` gap; a snapshot showing the action under way keeps it. A start settles when the workspace leaves its build, as a rebuild does, and a failed stop on its annotation. The first `open` refetches when a snapshot was requested before it (the server subscribes before answering). A detail `404` for a workspace the entities hold asks the list, which drops it. |
| `web/src/views`, `components`, `lib` | Home (`Running` + the catalog with a Clone or Start per row) and `/ws/:id` (state, the eight-step timeline, the feed as text, Start). `components/ActionButton.vue` is §4.2 for every mutating button: in flight until the settling event, *"no response yet"* after 10 s and never a failure, refusals by code, `in_progress` a note. `lib/workspaceCard.ts` is §6.1's table as a pure function of both halves: `supervisorHalf()` reads the reducer's supervisor entity (`supervisor.state` events, the views' `supervisor`), never `workspace.state`, and `cardStatus(w, fleet)` takes the fleet login (`stream.entities.identity`) for §6.6's override: a signed-out fleet leaves the card *Running* with no session action, and *"Waiting on Claude sign-in."* is `WorkspaceIdentityNote`'s alone, never also the card's line. A view with no `supervisor` field (a server older than Phase 5) keeps the workspace half. The environment link is built from an `env_…` id, never taken from the wire. `mocks/backend.ts`'s `supervisor: true` plays the session server (dev:mock sets it; specs default to false). `mocks/backend.ts` serves the workspace routes with the cap and the duplicate check; `scriptMode: 'manual'` holds a create's events for a spec to play with `playScript`. `components/ClaudeLogin.vue` is the login handshake (frontend §6.2) in Settings' Claude section, its state the reducer's `login` field (the GET's `login` and `auth.login`); on Settings the fleet banner drops its link, so its button is the one *Sign in to Claude*. The code is one component ref in an `<input>` in no `<form>`, shape-checked by `lib/login.ts` (whose spec reads the Go rule), cleared before the request, and swept by a canary spec; the mock's `loginMode: 'manual'` holds each phase for `loginReady`/`loginVerdict`. |
| `internal/web`, `web/` | The Vue 3 app in `web/`, embedded from `internal/web/dist`, which is **committed build output** — the Go build needs no Node, and `npm run check:dist` fails when it is stale. Never hand-edit `dist`; rebuild it. Every mutating button is `components/ActionButton.vue`, and a workspace's card action (Start, Stop, Rebuild, *Delete again*) is `components/WorkspaceAction.vue`, chosen by `lib/workspaceCard.ts`. Each mutation's in-flight mark is cleared by the event that *ends* it (`settles*` in `stores/workspaces.ts`), never by the `202` or the state move written before it — or, after a `resync` gap, by a snapshot that shows it ended (the same `OVER` predicate; see `web/src/stores`). Delete's confirm compares the typed name exactly and sends it as typed; a stuck delete's resume asks for nothing. `workspace.action` events are reduced into `Workspace.action`, and `liveAction` says whether a stop or delete is still running; a failed stop and a stuck delete are ended by the server's annotation, and `stopFailed`/`deleteStuck` read them — from the list alone too, via `lastAction`. The occupied count is **counted, not stored** (`lib/capacity.ts`, against the cap `GET /api/workspaces` carries), and `capacity.spec.ts` reads `Occupying` out of `internal/workspace/state.go` so the client's rule cannot drift. At the cap an action that would take a slot is replaced by `MakeRoom`, never disabled, and Running sorts stoppable cards first. The stream store keys its runtime by `toRaw(store)`: Pinia's devtools call actions through a fresh `Proxy`, and keying by `this` left every button spinning under `npm run dev:mock`. A stopped workspace waiting on a host-access request (design §6) shows `components/HostAccessApproval.vue` where its action would be — the added, changed and removed settings as JSON text, the warning (root on the host when `privileged` is asked), *Approve and continue* (sends the request's own hash) and *Cancel* — on the card, the catalog row and the detail view alike; the request is the reducer's `approval`, written only by `workspace.state` events and the views. |
| `web/src/views/secrets/`, `web/src/lib/secretRules.ts` | Phase 4's UI, one lazy chunk: the list, the write-only form (`reach` labelled with the literal question), grants with the `all_repos` confirm, the two-kinds rotate result, delete. Secret entities are written only by `GET /api/secrets` and `secret.*` events. The `PUT`'s 200 body is used for its `stale` lists and never applied. The fleet banner's undeliverable fault is the same kind of entity — from the list's `undeliverable` and the two delivery events, versioned — and `FleetBanner.vue` loads the list itself, since it is on every screen. The form's edit mode sends **no `value` key** when the value field is empty (keep the stored one), never `""`, and so never asks for the current value to change the reach. The value lives in one component `ref`, in a `<textarea>` (an `<input>` strips a pasted newline) inside no `<form>`, and a canary spec proves it is nowhere after submit. `secretRules.ts` mirrors `internal/secrets/validate.go`, and its spec reads that file, so **change a reserved name or a limit in Go and that spec fails until the copy matches**. **New secret never replaces**: it refuses a name the loaded list holds as it is typed, offers *Edit NAME instead*, and sends the `PUT` with `If-None-Match: *`, so a name another device stored meanwhile is the server's `412 secret_exists`, on the name field. The edit form sends no such header. |
| `deploy/Caddyfile`, `deploy/preview.caddy` | The entire LAN-facing surface, every value an env placeholder so the shipped files are the tested files. The preview site is a separate, optional file imported by glob, so a first deployment needs no wildcard certificate. |
| `deploy/install.sh` | The installer and upgrader, one file in two modes: standalone (`curl … \| sudo bash`) it downloads and verifies the release tarball and runs the copy inside; from the tarball it installs. Settings persist in `/etc/drydock/drydock.env`, parsed, never `source`d. Idempotent: files are written only when they change, and only what changed is restarted. **Everything that can refuse runs before anything is replaced**: the new binary is staged as `/usr/local/bin/drydock.new`, the Caddy config validated from a staging directory, and the master key decided (its `count-secrets` asked of the staged binary) before the binary, a key or a file is installed — v0.4.1 installed the binary first, so a refused key left the old process running a deleted file and the re-run called it current. An `EXIT` trap removes whatever was staged and, if the run stopped `drydock` for the key swap and did not reach `start_drydock`, starts it again. A `drydock` whose `/proc/<pid>/exe` is not the installed file (`-ef`: an interrupted run) is restarted, reported by the version it was really running, and copied as `drydock.previous` for the rollback; a run never says *current* unless the process runs the installed file. It requires Docker and the devcontainer CLI (Node 20+) on the service's `PATH` and installs neither; it adds `drydock` to the `docker` group — root by another name, design §13.4 — and gives the unit `ReadWritePaths=/srv/drydock/ws` and `RuntimeDirectoryPreserve=yes`, so `/run/drydock/sock/<id>/`, which running containers mount, survives a restart (`run.sh` checks its inode across one). The first install creates the secrets master key, `/etc/drydock/secrets.key` (32 bytes of `/dev/urandom`, `0400`, `drydock`'s, never printed), or installs the 32-byte file `--secrets-key PATH` names (a path, like `--github-app-key`, staged through a temp file in `/etc/drydock` and not kept in `drydock.env`); later runs keep it and **refuse rather than replace** one that is not a 32-byte file. A *different* `--secrets-key` replaces the installed key **only while no secret is stored** — asked with `drydock count-secrets` as `drydock`, once with the service running and again after stopping it, so nothing is written under the old key in between — and is otherwise refused with nothing changed and nothing restarted, the binary included. A `systemctl stop` that fails is reported, never a silent exit. Nothing ever deletes or re-seals a secret, and there is no `--force`. A rollback restores the previous unit with the previous binary. `--ui-host` (and `--preview-domain`) are **lowercased**, which also repairs an older `drydock.env`. A preview domain on the UI host's registrable domain — child, parent or sibling — is refused before anything is installed, by asking the bundle's own binary (`drydock check-preview-domain`) rather than reimplementing the Public Suffix List in shell. The final check — a `401` from `/api/auth/session` through Caddy — verifies TLS against `--ca-cert` (a private CA's certificate, kept in `drydock.env`; `--no-ca-cert` forgets it) or the system store, never `-k`; when only verification failed (curl exit `60`) a second, unverified request decides whether to say *"installed and running, but this host could not verify the certificate"* or report a real failure, and never turns a failure into a pass. The undocumented `DRYDOCK_VERIFY_CACERT` it replaces is gone; only releases before `--ca-cert` read it. |
| `deploy/package.sh` | Builds the release assets — the same script in CI and in the installer test. |
| `test/install/` | `run.sh` runs the installer against real systemd, the official Caddy package, Debian's Docker (a nested daemon) and the devcontainer CLI in a privileged container: refusal without Docker or the CLI, a real `devcontainer up` inside the running service's own mount namespace, fresh install, no-op re-run, upgrade, rollback, previews on and off, an unreadable key, a foreign Caddyfile, the secrets master key (created, never printed, kept across re-run and upgrade, a damaged one refused), `--ca-cert` (kept; without it the trust failure is named as one; with Caddy cut off from the socket the check still fails, with or without it), and a mixed-case `--ui-host` (lowercased, sign-in works, an old `drydock.env` repaired, `drydock serve` refusing one), a preview domain under or beside the UI host (refused, nothing changed), and last a first install *with* `--github-app-key` on a host with no `/etc/drydock`, which v0.2.0 failed because every earlier App install found the directory already made; then `--secrets-key` from nothing (a wrong size, base64, a missing file and a directory refused before anything is installed; the given bytes installed; the same key restarting nothing; a different key replacing it while no secret is stored, and refused with a secret stored — key byte-identical, drydock never stopped, the secret still served; a hex sweep of every installer output and the journal for the key bytes, the generated key included). Then what a refusal or failure must not leave behind: a key refused during an upgrade (`v0.0.2` stays installed and running from its own file; the runbook's re-run then upgrades), an interrupted upgrade (a replaced binary under a running process: restarted and reported, and onto a broken release rolled back to what was running), a failure after the key swap (drydock started again), a `systemctl stop` that fails (a `PATH` shim: reported, no staged key left), and a 0-byte database (refused, key unchanged). `live.sh vX.Y.Z` runs the README one-liner against a *published* release from GitHub; `live.sh --dir DIR vX.Y.Z` against assets on disk, served the way GitHub serves them, and then as the README's **upgrade**: the release *Latest* on GitHub, with a password and a stored secret, upgraded by those assets with no flags (the session and the secret must survive). Neither is part of `go test`; CI runs the first, the release workflow the second (`--dir`, on the draft's assets). |
| `test/container/` | The container tier: real Docker (the devcontainer's DinD, or the CI runner's) and the real `devcontainer` CLI. Adopt-an-orphan and died-unobserved against real containers, and Phase 3's deliverable end to end. That is a real `devcontainer up` with the Feature from this checkout and the workspace's broker directory bind-mounted. Inside, git pushes a `drydock/` branch through the helper; a push to `main` is refused, the other repository is unreachable, and there is no socket but its own (with another workspace's open beside it on the host) and no Docker socket. Then **a Drydock restart under the running container** — `CloseAll` and a fresh `Broker` on the same directory, a new socket inode (the old one held by a hard link, as a file mount would hold it, so a reused inode number cannot fake the check) — after which `drydock-broker PING`, a push and the prelude still work, and again over a symlink root in the container planted at the socket's name; mounting the socket file instead fails it with #16's exit 69. `TestLegacyBrokerMountAsDockerReportsIt` checks the legacy-mount reading against real `docker inspect`. Then Phase 4's: a suite run through the Feature's `CLAUDE_ENV_FILE` passes while a secret is granted and fails on the next command once it is not; a hostile value runs nothing; no value is in any `/proc/*/cmdline` or `docker inspect`. And Phase 2's: `POST /api/workspaces` through the real server, signed in, for a repository with a config and one without, both reaching `running` with the published Feature, the clones untouched and no token anywhere in the tree or the database; a secret granted to one reaches its `CLAUDE_ENV_FILE` prelude and not the other's. Each test runs under its own random label prefix. Skips without Docker unless `DRYDOCK_REQUIRE_DOCKER` is set, which CI does. The host-access gate (design §6): the tier's own `runArgs: --network=host` is pre-approved per repository (`approveHostNetwork`, hashed by the same function); a docker-in-docker repository stops `needs_approval` with no container, is approved through the API and comes up privileged; and the lifecycle test rewrites `devcontainer.json` from inside a running container with an `initializeCommand` that touches a host canary — the rebuild stops, the canary absent and a stale hash refused, and once approved it runs and the canary appears. |
| `test/browser/` | The browser tier (testing §10, §10.4): Playwright's Chromium against the real `drydock serve` and real Caddy on the shipped Caddyfile — that `drydock serve` with a **stand-in `docker`** on its PATH (fakeclaude where the login container's claude would be) and its own `--claude-volume`, so the tier drives the login handshake through a reload (`login.spec.ts`, §10.2 item 12) and can never reach the host's Docker or its credential volume — with a throwaway CA trusted through NSS in a per-run `HOME` — never `ignoreHTTPSErrors`. A tap between Caddy and each socket records what arrived, so "no cookie" is asserted at the server. Covers the `__Host-` cookie and its illegal variants, the `SameSite=Lax` split, the `Origin` belt, cross-origin reads, framing both ways, the CSP as served, SSE through Caddy's `encode`, the browser's own `Last-Event-ID` replay, sign-in with `return`, the mid-session `401`, and a wrong-host certificate refused. Mutation-checked. Run by `run.sh`, which resolves `@playwright/test` from `web/node_modules` via `NODE_PATH` and type-checks first. **Not Chromium-only**: `engines.spec.ts` also runs in Playwright's Firefox and WebKit (the `firefox` and `webkit` projects), driving the real sign-in form and Settings' sign-out and asserting the `Origin` the server received. v0.2.1 shipped a client that sent `Origin: null` from Safari and Firefox, which Chromium never shows. Those two engines cannot trust the tier's CA, so they run against a **loopback front** (`Stack.front()`: the real `drydock serve` over plain HTTP on 127.0.0.1) that maps only its own exact origin to the UI origin and passes `null` through, which the spec's controls prove from Node. The `signIn` fixture is a bare `fetch` (mode `cors`, so it sends the right Origin whatever the client does) and is for tests where how the sign-in was made is not the subject. It asks once more only when the tap shows the first request never reached the server: Chromium aborts in-flight requests with `ERR_NETWORK_CHANGED` when Docker adds or removes a veth beside the tier (testing §10.4). |
| `test/ansible/` | The Ansible companion's checks: `check.sh` extracts the document's YAML blocks and syntax-checks and lints them, then runs §7.1's journal assertion on localhost against journals given as data (an allowlist: a line it was never told about fails it); `live.sh` runs them against a bare Debian systemd container, installing a locally packaged release: first install, a `changed=0` re-run, an upgrade with its database backup, an App key rotation, a master-key backup that refuses a different key, and the move to a vaulted master key (the installed key vaulted: `changed=0`; a new one with no secret stored: replaced; another with one stored: refused, the key unchanged; the same refusal on a release change, leaving the old binary running; and the run after an interrupted upgrade, backed up by the running process's inode). Neither is in CI. |
| `test/docs/` | Checks on the operator documents: every `vX.Y.Z` in the README and `docs/deploy/` is a release (`CHANGELOG.md`) or the next one, computed by release-please's rule from the commits since the manifest last moved (plus `DRYDOCK_PR_TITLE` on a PR). #49 pinned v0.3.1, which release-please never cut. Skips in a shallow clone unless `DRYDOCK_REQUIRE_RELEASE_HISTORY` is set, which CI does. |
| `test/component/` | Real binaries, nothing mocked. Today: the Caddyfile conformance test (testing §3.2), mutation-checked against the Caddyfile itself. |
| `test/fixtures/` | The corpus: 49 fixtures from `2.1.289` and devcontainer CLI `0.89.0`, plus `record.sh`, which is testing §11.1 step 3. Some are hand-written or synthetic, and their `.meta` says which. |

Three things about that code worth knowing before extending it:

- **Handlers are `nil` for routes a later phase owns**, and `Build` mounts a `501`. That is deliberate:
  the table is the complete contract from the first commit, reviewable against §5 as a list, and the
  gate is provably applied to routes nobody has written yet. **The auth gate runs before that `501`**
  — otherwise an unauthenticated caller could tell a declared route from a nonexistent one and read
  the API surface off a server it cannot use. There is a test for the ordering; do not reorder it.
- **Classifiers refuse rather than guess.** A classifier that quietly returns `IdentityOK` for bytes it
  cannot parse is exactly the failure the corpus exists to prevent, so each returns an error for input
  it cannot read — an unreadable credential file is never `absent`, a URL fragment is never usable.
  The identity classifier also decides `blanked` from the file *before* reading `auth status`, so the
  less stable input can never hide the fleet-wide failure.
- **The meta-tests were mutation-checked**, not just observed passing: moving the `501` ahead of the
  auth gate fails 20 subtests, marking a route unauthenticated fails the count assertion by name, and
  deleting the `Origin` check fails 14. Keep that property — a negative test nobody has watched fail
  is a negative test that might be vacuous (testing §4.1).

Two of those docs ship something runnable, and both are the thing to open before arguing about the
subject in prose. `docs/design/frontend/prototype/prototype.html` is a clickable harness for every
non-trivial UI surface at 360 px, with a switch for the fleet-wide login state; the frontend
document's figures are distilled from it. `docs/design/brand/icon-preview.html` renders every icon
form on both grounds down to 16 px beside the Claude and GitHub favicons, with the measured
clearances and contrast ratios. Both are single self-contained files — open them in a browser.

The devcontainer (`.devcontainer/devcontainer.json`) carries the full toolchain: Go (with
golangci-lint), Node, **docker-in-docker**, the `devcontainer` CLI, Caddy, `gh`, and
`sqlite3`/`socat`/`nc`/`jq`.

**The repository is two languages now, and that was decided rather than drifted into.** The frontend
design settles on Vue 3 + TypeScript + Vite, so `npm`, a `node_modules`, and a TypeScript toolchain
are here whether any other part wants them or not — which is what withdrew the testing plan's
`chromedp` fallback in favour of Playwright, since the cost it was being charged for was already
paid. Node is present for the `devcontainer` CLI, so Vitest needs nothing new.

Two things the browser tier needs, settled by Spike 04. **`libnss3-tools` is now in the apt feature**
— `certutil` is how a test CA gets into Chromium's trust store, and that is the only route that
keeps certificate validation on. **Playwright's browsers are deliberately not installed by the
devcontainer's features**: `--with-deps` pulls in some forty transitive system libraries, and
enumerating those in a package list is how the list goes stale silently. It is a lifecycle command
instead — `postCreateCommand` runs `npm ci` in `web/` and then `npx playwright install --with-deps
chromium firefox webkit`, which fetches the builds web/'s pinned `@playwright/test` wants: Chromium
for the whole tier, Firefox and WebKit for `engines.spec.ts`. A `drydock-playwright-cache` volume on
`~/.cache/ms-playwright` keeps that download (~114 MB for Chromium, about 100 MB more for each of
the others) across rebuilds, the same reasoning as the DinD volume beside it. **The `postCreate` step needs a container
rebuild to take effect**; until then, run the two commands by hand in `web/` (the system libraries
need `sudo npx playwright install-deps chromium firefox webkit`). Playwright itself is a devDependency of `web/`,
pinned — bump it with the §11.6 ritual, since a new Chromium is a new set of cookie rules.

A third volume, `drydock-gh-config` on `~/.config/gh`, keeps `gh`'s login across rebuilds. The intended
login is a **fine-grained token scoped to this repository alone** (`! gh auth login --with-token`),
not the default OAuth login, which reaches every repository the account can. The directory is `0700`
because `hosts.yml` holds the token in plain text — there is no keyring here. Like the Playwright
volume, it makes Docker create its parent (`~/.config`) as root, which `postCreateCommand` chowns back.

`devcontainer-lock.json` is a **generated artifact — never hand-edit it.** The CLI regenerates it
from the resolved feature set on every build, so an added feature needs no lock entry: leave it out
and the tag is resolved fresh. The digests in it are trusted *input* during resolution (they pin
what actually gets fetched), so a hand-written wrong digest silently installs content the tag no
longer points at. Adding a feature means editing `devcontainer.json` only.

Docker-in-docker rather than docker-outside-of-docker is a deliberate choice, not a default. Drydock
hands the daemon host paths to bind-mount — the clone at `/srv/drydock/ws/<id>/repo` and the broker
socket — and under DooD the daemon is the *host's*, so those paths resolve differently inside and
outside this container and every mount breaks. That is the same path-identity problem §1 gives as
the reason Drydock is not containerized in production. With DinD the daemon lives in here, so a path
means the same thing to both sides. Consequences worth knowing: the inner daemon's
`/var/lib/docker` is a named volume so images survive a rebuild (only ever run one such container at
a time), and containers Drydock creates are invisible to the host's `docker ps`.

Read `docs/design/overall/drydock-design.md` before making architectural decisions. It is dense and
opinionated, and most "why is it like this?" questions are answered there with reasoning that is
easy to lose. When a change contradicts it, update the doc in the same change rather than letting
the two drift.

## Releases

**Commit subjects are conventional commits, and that is load-bearing.** release-please reads them
off `main` (PRs are squash-merged, so it is the PR title, which `pr-title.yml` checks): `feat:` and
`fix:` cut a release, `feat!:` a breaking one, and everything else — `docs:`, `test:`, `chore:`,
`ci:`, `refactor:` — does not. A design-doc change is `docs:` even when it is large.

Merging release-please's PR tags `vX.Y.Z` (no component prefix: one root package) and creates the
release **as a draft** — `"draft"` plus `"force-tag-creation"` in `release-please-config.json`,
because GitHub tags a draft only when it is published and release-please finds its previous release
by the tag's commit. In the same workflow a `test` job runs the Go suite and `assets` uploads them
under **fixed names** so `releases/latest/download/<name>` always resolves: `drydock_linux_amd64.tar.gz` (binary,
`install.sh`, both Caddy files, `VERSION`), `SHA256SUMS`, and `install.sh` stamped with its tag. It
is one workflow, not a tag-triggered second one, because tags pushed with `GITHUB_TOKEN` trigger
nothing. The repo setting *Allow GitHub Actions to create and approve pull requests* must be on.

After the upload, `verify` downloads the draft's assets back (`gh release download`; a draft needs
`contents: write` to see) and runs `test/install/live.sh --dir` — the README line against those
bytes, served locally through `DRYDOCK_DOWNLOAD_BASE`, on a fresh host and as an upgrade of the release *Latest* now. `verify` takes `test/install` from the workflow's own commit, as `test` takes `.github/actions`, so a resume picks up a fix to either. Only then does `publish` make the release
public and *Latest*, and check that `releases/latest/download` serves it, returning it to a draft if
not. So a red job anywhere leaves a draft and the previous release still *Latest*: never a *Latest*
with no files. Re-run the failed jobs (`gh run rerun --failed`) only for a transient failure: a
re-run uses the workflow file its run started with. When the fix is to the workflow or its test
environment (`.github/`, `test/install/`), merge it as `ci:` and resume the existing draft with the fixed workflow —
`gh workflow run release-please.yml --ref main -f tag=vX.Y.Z` (`workflow_dispatch` skips
release-please and refuses a tag that is not a draft). Fix forward with a `fix:` when the code is wrong.
The release PR itself gets no CI (a `GITHUB_TOKEN`-opened PR triggers no workflows), so it merges
only with an admin bypass of the ruleset; that is a token decision for the owner, not a workflow bug.

**CI** (`.github/workflows/ci.yml`) runs on every PR and push to `main`: `gofmt`, `go vet`, `go test`
with Caddy, `socat`, `nc` and the devcontainer CLI installed and `DRYDOCK_REQUIRE_CADDY=1`,
`DRYDOCK_REQUIRE_DOCKER=1` and `DRYDOCK_REQUIRE_TRANSPORTS=1` — without them a missing `caddy`, Docker,
or `socat`/`nc` (the broker's argv sweep) is a *skip*, which in CI is a silent pass — then `npm run check`, then `test/install/run.sh`, and
the `browser` job: `test/browser/run.sh` after `npx playwright install --with-deps chromium firefox webkit`,
`libnss3-tools` and the pinned Caddy, uploading Playwright traces when it fails. **The Go suite's tools and
commands live in one composite action, `.github/actions/go-suite`**, which CI's `go` job and the
release's `test` job both run (and `browser` for Go and Caddy alone), so a release is held to exactly
what every PR passed. They used to be two copies, and v0.2.0's release failed because `socat` had
reached only one of them. Add a test dependency there, never to one workflow. The ruleset requires a
check named `go`, which is why it is a composite action rather than a reusable workflow (that would
rename the check `go / …`). The `go` job checks out with `fetch-depth: 0` and passes the PR's title, because `test/docs` reads the commits since the last release: **a deploy document may name only a released version (a `CHANGELOG.md` heading) or the one release-please will cut next** from those commits and the title, so a doc pinned to a patch that a `feat:` turns into a minor fails that PR. Pin the docs to the release your PR will cut; `vX.Y.Z` for an example. Its Caddy version is pinned; the devcontainer's Caddy feature is not, so
bump the pin when a rebuild moves it. Releases are amd64 only, by choice.

**The GitHub contract tests** (`.github/workflows/github-live.yml`) run the `TestContract*` functions
against the real dev App, `krelinga-drydock-dev` (App ID 5189839), which is installed on
`krelinga/drydock-testbed-a` (declares a dev container) and `-b` (does not) and nothing else. The same
functions run against `githubtest`'s fake on every `go test`, through `githubtest.NewBackend`. So when
the live run fails and the fake run passes, the fake holds a belief about GitHub that is wrong, and
the failing test names it. The job runs on PRs, on `main`, nightly and on demand. It needs the
repository secret `DRYDOCK_DEV_APP_KEY` (the `.pem`) and the variable `DRYDOCK_DEV_APP_ID`.
`DRYDOCK_REQUIRE_GITHUB_LIVE` stops it quietly testing the fake. **The testbed repos are fixtures**:
change one and you must change `githubtest.Testbed()` too; both must stay private, or the scoping
tests test nothing. So are the dev App's permissions (`githubtest.DevAppPermissions()`, asserted
against `GET /app`): production's set, every permission §9.3's scopes ask for, `actions: write`
included, so the broker's contract mints both scopes live. A missing permission is pinned live by
asking for `administration: write`, which the App must keep lacking.

The README's one-liner is the contract: change an asset name, a flag, or `/etc/drydock/drydock.env`
and an existing install's re-run is what breaks. `docs/deploy/first-deployment.md` is the operator's
runbook for a first real deployment: every command, path and message in it is taken from
`deploy/install.sh` and the server as built, so a change to either — a flag, a file the installer
writes, a journal line, a UI label it tells the operator to press — updates it in the same change.
Its *Known issues* list is installer gaps found while writing it; fixing one means deleting its entry. `docs/deploy/first-deployment-ansible.md` is its Ansible companion, the same steps as tasks an operator copies into their own playbook: **a change to the installer updates both documents**, since its tasks pass the installer's flags, set the password through `drydock passwd --if-unset`, and read `changed` off the installer's `==> ` lines. `test/ansible/check.sh` assembles its `# file:` blocks and runs `ansible-playbook --syntax-check` and `ansible-lint`; `test/ansible/live.sh` runs the assembled play against a bare systemd container (needs the internet, not in CI). `test/install/run.sh` installs from a local copy of
the release assets via `DRYDOCK_DOWNLOAD_BASE`, so it exercises the one-liner's path.

## What Drydock is

A single Go binary on one dev server that turns a GitHub repo into a running dev container with a
supervised `claude remote-control` session inside it, driven from a web UI on the LAN. One click
clones the repo to host disk, brings up its dev container with an injected Drydock feature, and
starts a session server reachable from the Claude app — with GitHub push credentials scoped to that
one repo.

## Architecture in one pass

Everything is one process plus subprocesses; the only pieces that are more than glue are the
**session supervisor** (owns long-lived PTYs) and the **token broker** (the only component that ever
holds a GitHub credential).

- **Caddy** owns the only LAN-facing listener (`:443`, strict host matching) and forwards to
  Drydock's Unix socket. It knows nothing about Drydock.
- **API server** — REST + one SSE stream + the UI, over a Unix socket. Every mutating route is
  async: validate, write a state transition, return `202`, let the client follow `/api/events`.
- **Container manager** shells out to the `devcontainer` CLI for every container operation and finds
  containers again by `--id-label drydock.workspace=<id>`. Never scrape `docker ps`; parse the
  single JSON object `devcontainer up` prints on stdout. There is **no `--json` flag** — passing one
  fails — and the result names no step, so per-step events come from Drydock's own tracking.
- **Workspace manager** owns the host bind-mounted clone at `/srv/drydock/ws/<id>/repo` (a real git
  repo, so `remote-control --spawn worktree` works). Clones survive container rebuild and delete.
- **Session supervisor** runs one `claude remote-control` process per workspace serving *many*
  sessions, with restart backoff and a log ring buffer.
- **Token broker** holds the GitHub App private key and listens on one Unix socket per workspace,
  bind-mounted into that container only.
- **SQLite (WAL)** beside the binary. See §4 of the design doc for the schema.

### The two ideas the rest hangs on

**Socket-as-identity.** Drydock binds no TCP port, and neither does the broker. The socket a request
arrives on *is* the claim of who is asking and which repo they may touch — so the broker protocol
carries no repository parameter (`GET-TOKEN scope=git`, `GET-SECRETS` with no arguments). A
compromised container can only ask for what it already has. Removing the socket mount removes GitHub
access instantly, with nothing to revoke.

**Docker is the truth, the database is the cache.** On startup, reconcile containers found by
`label=drydock.workspace` against the `workspace` rows in one direction only. Adopt orphans rather
than killing someone's work; never auto-start what was stopped. Every mutable container fact in the
DB must be rebuildable from labels.

## Invariants — do not break these without changing the design doc

These come from §2 (Claude Code constraints) and §13.5 (non-negotiables). Most of them fail
*silently*, which is why they are listed rather than left to judgment.

- **No TCP listener.** Not `0.0.0.0`, not `127.0.0.1`. Unix socket only, group-owned, Caddy the only
  member.
- **Caddy's admin API is never left on `localhost:2019`.** That default is a loopback port any local
  process can reach, and whatever can reconfigure Caddy — the one member of the socket's group — can
  point it at Drydock on its own terms, undoing the argument for the socket. `deploy/Caddyfile` puts
  it on a `0600` Unix socket; the conformance test asserts nothing listens on 2019.
- **Remote Control needs a real `claude auth login` credential**, not `CLAUDE_CODE_OAUTH_TOKEN` —
  a setup token can only make model requests. This is why the PTY login handshake (§7.2) exists.
- **`.credentials.json` alone is not enough.** Remote Control also needs the `oauthAccount` record
  from `.claude.json` in the same directory — without `organizationUuid` it refuses to start even
  though the token works fine for model requests (Spike 02). Sharing the whole `CLAUDE_CONFIG_DIR`
  carries it; copying the credential file does not, which is why that option is struck, not merely
  second-best. Two more keys in that file gate a headless start: `hasTrustDialogAccepted` for the
  workspace path, and `remoteDialogSeen` — without the latter the server *hangs* on an interactive
  `Enable Remote Control? (y/n)` prompt rather than failing. The feature writes those two;
  `oauthAccount` comes only from a login, and the feature never writes it.
- **These variables must stay unset in every container:** `ANTHROPIC_BASE_URL` (or point at
  `api.anthropic.com`), `DISABLE_TELEMETRY`, `DO_NOT_TRACK`,
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `DISABLE_GROWTHBOOK`. Any of them disables Remote
  Control while everything still builds and starts. They are also on the reserved-secret-name list.
- **Auth is middleware around the whole mux**, so a route added later is protected by default. Only
  the sign-in POST is unauthenticated, and it is rate-limited and lockout-guarded.
- **The `Origin` check is exact-match and fails closed** (design §13.5). Every mutating route
  compares `Origin` to the literal UI origin — never a suffix, never a registrable-domain match, never
  case-folded — and refuses one that is absent, empty, `null`, or sent twice exactly as hard as a
  wrong one. The API sends no permissive or `Origin`-reflecting CORS. It is the belt behind
  `SameSite=Lax`, which does the primary CSRF work only because previews are on a *different
  registrable domain* (`config.CrossSite`); a same-site preview domain is refused at startup and by
  the installer, since `SameSite` would then let a preview's requests carry the cookie.
- **Nothing returns or pre-fills a stored secret value** — no route, response, form or event, and
  no reveal button or re-auth escape hatch either. A secret's metadata (reach, description) can be
  edited, and a *new* value can be written over the old one through a field that opens empty; what
  is prohibited is the stored value ever travelling back out. The schema has no `value` column, and
  it should stay that way.
- **A secret value is single-line on write and single-quoted on delivery.** Control characters are
  refused by the API (§10.1); `drydock-secrets export` quotes every value, escaping `'` as `'\''`
  (§10.3). A newline forges a line in the `GET-SECRETS` response and bypasses the reserved-name list
  above; an unquoted value is shell code that runs before *every* Bash command, because the prelude
  runs per command. The client also fails the fetch when `count=` disagrees with the lines received.
- **The workspace label prefix is configuration, recorded at first run, and reconciliation refuses a
  foreign one.** Adoption and deletion are label-driven, so a second instance sharing the prefix
  inherits the first's containers *including its delete path*. The SQLite advisory lock does not
  cover this — the second instance has its own database. This is what a test run is.
- **Store no credential but the App private key.** GitHub tokens live in a bounded in-memory cache;
  `token_grant` records that a token was issued, never the token. `auth_session.id` is the SHA-256
  of the cookie value, so a stolen DB file yields no usable cookie. Secret values are XChaCha20-
  Poly1305 ciphertext with the secret id as AAD.
- **The App key and the secrets master key never enter the environment** — mode `0400` files read
  once at startup. Environment variables leak into `/proc`, crash reports, and every child process.
- **No host-affecting setting runs without an operator approval that covers it.** The clone is
  bind-mounted read-write, so the `devcontainer.json` a start or rebuild reads may be the
  container's work, and `up` runs `initializeCommand` on the host as `drydock` (who can read both
  keys and is in `docker`) and hands `runArgs`, `mounts`, `privileged` and the rest to the host's
  daemon. Step 3 reads the resolved **and merged** configuration (Features and image metadata can
  ask for `privileged` and mounts too), computes the host-access subset by the allowlist in design
  §6 — an allowlist, so a field a new CLI adds is in it by default — and runs a non-empty subset only
  if every entry is within the repository's current approved set (`container.Covered`: the same
  value, or for `capAdd`/`securityOpt`/`mounts` fewer elements — never `runArgs`, which is argv);
  running less never narrows the approval; otherwise the run stops
  `needs_approval`, stopped, before any `up`. The workspace's containers are stopped first so
  nothing rewrites the file between the check and `up`. `read-configuration` was measured not to run
  `initializeCommand` (the hostile fixture's recording checks a host canary). Be honest about what
  approval is: an approved `privileged` (docker-in-docker) is host root, and an agent in it escapes
  without touching the config — the gate guarantees only that a container cannot *grant itself* host
  access by rewriting its config. Still open: a mutable Feature or image tag can serve different
  metadata to the check and to `up` (§6).
- **No Docker socket in any workspace container.** Docker-out-of-Docker would let one container
  mount another's broker socket.
- **The frontend invents no state.** Every mutation returns `202`; the only local state a click may
  create is "a request is in flight". Entity state is written by exactly one thing — the reducer over
  the SSE stream — and the `202` response body is discarded rather than applied. Patching an entity
  from a mutation response is the failure that looks like it works: it is right most of the time and
  diverges the moment a second device acts. Nothing is ever framed, either: the UI sends
  `frame-ancestors 'none'` and `frame-src 'none'`, so a preview can neither embed the control plane
  nor be embedded in it. See `docs/design/frontend/frontend-design.md` §2.1 and §8.
- **Redact by default.** Passwords, login codes, session cookies, GitHub tokens, secret values, and
  PTY buffers never reach the event log, a file, or Caddy's access log. Note the login prompt does
  *not* echo, so the PTY buffer does not actually contain the one-time code (Spike 01) — but Drydock
  receives that code over HTTP and holds it in memory, where a request log or a crash dump can still
  leak it, and non-echoing is undocumented behavior of a pinned version. Redact the code, and still
  never store the PTY buffer verbatim.
- **`drydock-secrets export` must be silent on success, and must `exit` non-zero on failure.** It
  runs as a shell prelude before *every* Bash command (Spike 03), so anything it writes to stdout or
  stderr is prepended to every tool result the agent reads for the rest of the session, and an
  `exit` is what makes a broker outage fail the command loudly instead of running tests with the
  secret silently missing. Diagnostics go to Drydock's event log over the socket. **Its exit status
  alone does nothing**: `eval "$(…)"` evaluates the empty string a failed substitution leaves and
  succeeds, so the helper also prints `exit 69` for the `eval` to run. A test of this asserts the
  command *after* the prelude did not run — a test of the helper's status passes while the bug
  stands.
- **The `CLAUDE_ENV_FILE` script is one constant line that delegates to the helper:**
  `eval "$(drydock-secrets export || echo exit 69)"`. Its *text* is cached per session and is passed to every
  command shell as `argv` (Spike 03). So a text change needs a supervisor restart, and resolved
  values must never be inlined — the helper is *invoked* from the prelude, which is the only reason
  values stay out of `ps`. The `|| echo exit 69` is not decoration: a helper that cannot run at all
  (missing, not executable) prints nothing, and without it `eval ""` succeeds and the command runs
  without its secrets.
- **Stop a `remote-control` server with `SIGTERM`, escalating to `SIGKILL` only on timeout.** A clean
  stop deregisters the folder and lets the next start in immediately; a `SIGKILL` of a server with
  no live session blocks the next start for one to three minutes (Spike 02). That block is a wait,
  not a crash, and must not consume the restart budget. **Match it on `already served by a
  terminal`, never on `409`** — `2.1.246` prefixed the message with the status code and `2.1.289`
  dropped it, so a classifier keyed on the number silently reclassifies the one retryable refusal.
- **Two of the three config gates *hang* rather than fail.** A missing `remoteDialogSeen` waits on
  `Enable Remote Control? (y/n)`, and a missing trust record waits on `Trust <dir>? [y/N]` — the
  latter only on a PTY, which is exactly what the supervisor gives it; redirected, the same case
  exits `1` with a message. A hang has no error string to assert on, only an absence, so those
  scenarios assert a **timeout** and a suite with no hang fixture passes against the real bug.
- **Pin the Claude Code version and set `DISABLE_AUTOUPDATER=1`** in the devcontainer feature. The
  feature carried *no* pin until 4 Oct 2026, and the rebuild that day silently moved `2.1.246` →
  `2.1.289` and falsified three recorded findings — the URL-wrapping claim, the `409` signature, and
  trust-fails-fast. It is pinned now; keep it that way, and bump it through the §11.1 ritual rather
  than by rebuilding. Two places scrape Claude Code's terminal output (the login URL, the session
  URLs); a background update changes them without warning, and every spike measures undocumented
  internals of one version. **Current state: all four Claude Code spikes are re-measured on
  `2.1.289`.** `00`
  reproduced in full — the lock is still a directory in the shared volume, 16,335 reads across a
  live write with zero torn, and the tombstone is byte-identical to the recorded fixture.
  **Re-run all four Claude Code harnesses (`00`–`03`) on every bump** and update the version here,
  in the Feature, in `internal/classify`, in `internal/claudetest` (with its pins), and in each spike report. Spike `04` is about browser
  behaviour and has its own trigger (testing §11.6).
- **The shared credential volume must be a local Docker volume — never NFS or CIFS.** Claude Code's
  cross-container refresh lock is a `mkdir(2)`-based lockfile at
  `$CLAUDE_CONFIG_DIR/.oauth_refresh.lock`; network filesystems do not give `mkdir` the atomicity the
  whole guarantee rests on (Spike 00). Enforced twice: step 4 refuses a volume with a non-local
  driver or driver options, and `drydock-preflight` refuses a network filesystem type at
  `CLAUDE_CONFIG_DIR`. Nothing in Drydock removes the volume.
- **Anything Drydock writes into the shared `.claude.json` takes Claude Code's own lock.** Claude
  Code (read out of `2.1.289`) saves it under a proper-lockfile `mkdir` lock at
  `$CLAUDE_CONFIG_DIR/.claude.json.lock` and re-reads under it; `drydock-claude-config` does the same,
  then writes temp + `rename(2)`, merging rather than rewriting so `oauthAccount` survives. It waits
  and never steals the lock, for the reason below.
- **Never reap `.oauth_refresh.lock`.** An abandoned lock self-heals after 60s, and a cleanup pass
  racing the 5s heartbeat is strictly worse than waiting. Verified in Spike 00.
- **A blanked credential is not an expired one.** On a dead login Claude Code rewrites
  `.credentials.json` in place with `accessToken: ""` / `refreshToken: ""` / `expiresAt: 0`, which
  kills every container on the volume at once. The expiry watch must report it as "signed out, sign
  in again" rather than folding it into the three-day expiry countdown (§7.3).

## Working conventions from the design

- **Secrets are default-deny.** No `secret_grant` row, no secret. The required `reach` field ("what
  can someone do with this?") is a real control, not documentation.
- **Sessions are observed, not owned.** Drydock never creates a Remote Control session; it tails
  `--verbose` output continuously and upserts `rc_session` rows as sessions appear. If the cache
  drifts, the Claude app is right and Drydock is wrong. The UI links out with a count; it does not
  reimplement a session browser. The handle to store is the **environment id** (`env_…`, one per
  workspace, survives restart) and the link is `claude.ai/code?environment=<id>`; `Capacity: N/4`
  gives the count for free. Scrape **ids**, not URLs, and take them **only from OSC 8 hyperlink
  targets** — the URL and its label run together in the byte stream (Spike 02), and a bare
  `session_…` match would also accept an id the model printed in its own prose. The terminator is
  BEL on `2.1.289`, ST on `2.1.246`.
- **Agent branches go under a `drydock/` prefix**, configured in the feature rather than left to the
  model to remember. Commits use the App's bot identity.
- **Nothing stops a workspace automatically.** No idle reaper — distinguishing "idle" from "an agent
  thinking" wrong destroys work. Capacity is a hand-managed cap plus a stop button.
- **Every step of clone → container writes an event**, so a failure names its step rather than
  reporting "failed".
- **A fault with one cause gets one message and one button, wherever it manifests.** A blanked
  shared credential takes every workspace down at once, and rendering each card from its own row —
  the obvious implementation — produces ten *Restart session server* buttons, none of which can
  work, with the one genuinely different fault buried among them. So fleet-wide identity state
  overrides per-card presentation, and the override stays narrow: only what the credential actually
  broke is replaced, and a card whose image build failed keeps its own status and its own action. A
  per-card action that cannot work is never shown disabled; it is replaced by the action that can.
  Frontend design §6.6.
- **`blanked` and `absent` are different sentences.** *"Signed out. Sign in again."* means everyone
  just lost access; *"No one has signed in yet."* is the expected first-run state. `auth status
  --json` reports `loggedIn:false` for both and only the credential file separates them (§7.3), so
  rendering them alike is how a routine first run and the worst failure in the system end up looking
  identical.

## Build order

§14 of the design doc orders the phases so the riskiest unknown resolves first. Follow it:

0. **Spikes — all four done.** `00` shared credential volume under concurrent refresh: safe. `01`
   scripted PTY login handshake: scriptable, and `claude auth status --json` removes the expiry
   watch's terminal scraping. `02` supervisor restart survival: a plain restart reconnects the same
   environment *and* sessions, plus three `.claude.json` keys gate a headless start. `03`
   `CLAUDE_ENV_FILE` is re-read and re-executed once per Bash command. Reports and re-runnable
   harnesses in `docs/design/spikes/`. Two things stay unverified on purpose, both parked for Phase
   5: the `Login successful` match (needs a human in a browser — run `harness-01-login/run.sh login`)
   and whether a `--spawn worktree` path needs its own trust record.

   **A fifth spike, `04`, is also done** — the browser tier's local CA. A headless Chromium trusts a
   throwaway CA via `certutil -A` into `~/.pki/nssdb`, and all fourteen assertions pass with no
   `ignoreHTTPSErrors`: `__Host-` accepted and replayed, its three illegal variants refused, a
   cross-site `POST` and `GET` cookieless, and a cross-site *top-level navigation* carrying the
   cookie — which is the measured justification for `SameSite=Lax` over `Strict`. The browser tier
   is built as specified, in `test/browser/`: every §10.2 item whose subject exists is built, and what waits on
   previews or Phase 5 (items 6, 8 and the handshake half of 5) is listed in testing §10.4.

   Keep `ignoreHTTPSErrors` banned, but for the right reason: it does **not** break cookie semantics
   (it passes the same fourteen). What it breaks is the tier's ability to notice a *misissued*
   certificate — and so does `--ignore-certificate-errors-spki-list`, which is otherwise a fine
   no-system-state fallback. Only real NSS trust still refuses a cert served for the wrong host.

1. **Front door** — socket listener, `drydock passwd`, session middleware, `Origin`/`Host` checks,
   Caddy block. *Nothing else gets built until every route without a cookie returns 401.* **Server side
   done:** every API route is gated end to end against the real server over a real socket, and the
   Caddyfile is tested under real Caddy. **Still open for Phase 1:** the frontend shell (sign-in view,
   the `401` path, the embed pipeline) and a real deployment, since "sign in from your phone over
   HTTPS" needs the externally provisioned certificate.
2. **Walking skeleton** — repo list, clone, `devcontainer up`, states, SSE, boot reconciliation.
   **Server side done:** `POST /api/workspaces` runs all eight steps to `running` (steps 4 and 8 are
   recorded no-ops until Phase 5), in the container tier for a repo with a config and one without.
   Still open: the reboot drill against a real deployment.
3. **Credentials** — token broker, per-workspace socket, git credential helper, `gh` shim.
4. **Secrets** — encrypted store, `GET-SECRETS`, `drydock-secrets export`, grants UI. **Backend and
   container side done** (`internal/secrets`, the routes, the verb, the client, the Feature's env
   file, the installer's master key); the grants UI is still open.
5. **Claude** — shared credential volume, login handshake, supervisor, session discovery, expiry.
   **Expiry watch done**, server and UI (`internal/identity`, `GET /api/auth/claude`, the fleet
   banner): the banner's one *Sign in to Claude* leads to Settings' Claude section.
   **Login handshake done**, server and UI (`internal/login`, the three `/api/auth/claude/login`
   routes, `auth.login`, `components/ClaudeLogin.vue` in Settings): a short-lived login container
   on Drydock's own PTY, tested against fakeclaude on a PTY, in a real container and through
   Chromium. **Still unverified on purpose:** a real `Login successful` — it needs a human in a
   browser (`docs/design/spikes/harness-01-login/run.sh login`, or Settings on a deployment).
   **Done:** the Feature's Claude half (pinned install, `CLAUDE_CONFIG_DIR`, preflight assertions,
   the two hanging `.claude.json` keys) and step 4's volume — proven in `test/container`, where five
   workspaces share one volume and each runs the pinned `claude`. **The supervisor and session
   discovery are done** (`internal/supervisor`, the two routes, the card's supervisor half): tested
   against `fakeclaude` and through the real CLI and Docker. Still open: the done-when on the owner's
   real account, which also answers how long a real access token lives (design §15.1) — and so
   whether `expiring`'s countdown means anything.
6. **Livability** — stop/rebuild/delete, concurrency cap, disk and session counts, log viewer.
   **Server side of stop, rebuild and delete done**, with boot reconciliation resuming a delete and a
   container-tier test through provision → stop → start → rebuild → delete. Their buttons, the
   session count (the capacity fraction), the log viewer and Stop's live-session confirm are built.
   Still open: disk and memory on the card, and the rest of §12's messages.

Phases 2–4 are independently useful; if Phase 5 is blocked by something in §2, what remains is still
most of the value.

## Testing

`docs/design/testing/testing-design.md` is the plan. Four tiers — unit, component (fake subprocesses,
real sockets and SQLite), container (real DinD), browser (real Chromium and Caddy) — plus rituals
triggered by an event rather than a commit. A test lives in the cheapest tier whose reach includes the
boundary its assertion is about. Six things from it change how code gets written here:

- **Every negative test carries a positive control in the same function.** Nearly every invariant
  above is a prohibition, and a prohibition is satisfied by a binary that does nothing. A test that
  would still pass with the feature deleted is not a test of the invariant.
- **The canary sweep.** Component tests keep all mutable state under one temp root, seed
  high-entropy canaries, and grep the whole tree plus the SQLite file's raw bytes afterwards. This is
  how "redact by default" and §4's four deliberate schema absences are covered without a sink list
  anyone has to remember to extend. Two sinks are not files and are swept explicitly:
  `/proc/<pid>/cmdline`, because the prelude's text is `argv` on every command, and the HTTP path
  that carries the login code.
- **The route table is data**, not `mux.HandleFunc` calls — `{method, pattern, handler, mutating}` —
  so the auth, `Origin`, CORS, and two-mux-separation meta-tests enumerate it and a route added later
  is covered without editing a test.
- **Time, disk, and randomness are injected**, and subprocesses resolve by `PATH` so a fake binary
  can stand in. A Go mock of the `devcontainer` CLI tests our belief about it; a fake binary tests
  the argv we actually build, which is a security surface.
- **To prove a value is in no process's argv, trace it.** A wrapper around `git` sees only git's
  own argv; `GIT_TRACE` from that wrapper also records the processes git starts, such as a
  credential helper, which is where a token put in the wrong place shows up (`internal/clone`).
- **The `drydock.workspace` label key is configuration, not a constant.** Reconciliation adopts and
  deletes by label, so a second Drydock on the same daemon — which is what a test is — will adopt and
  destroy real workspaces. The SQLite advisory lock does not prevent this.

Two testing consequences of the Phase 0 findings are worth having here, because both are failures a
correct-looking test passes. **Exit status is not a discriminator** — all four `remote-control`
startup refusals exit `1`, so the classifier must read the message, and its test feeds four fixtures
with the same exit code and demands four verdicts. And **a missing `remoteDialogSeen` hangs rather
than failing**, so that scenario asserts a *timeout*; a suite with no hang fixture passes against the
exact bug Spike 02 found.

§15 of that document recorded five findings that were changes to the *design* docs rather than to it.
**All five are now applied** (overall v7, port-forwarding v4) and §15 is a dated record rather than an
open list: secret values are validated and quoted (the two invariants above), the three missing
columns exist (`claude_identity.state`, `workspace.environment_id`, and `waiting_registration` in
`supervisor.state`), preview ports are retired rather than deleted so a slug is never reissued, and
the label prefix is configuration. Each has test rows in §8.2, §8.5 and §8.6 — a finding closed in
prose with no test is one that reopens quietly.

## Brand

`docs/design/brand/brand-design.md` settles the icon at v1.0 and records why each alternative lost.
The mark is a section through a **drained** basin holding a container clear of the floor on keel
blocks, `DD` stencilled on its face — the drained basin is the only idea it carries, which is why
drawing water in it (every "harbour" variant) was rejected, along with anything Docker-adjacent,
since the design treats Docker as an implementation detail it never scrapes. Phase 1 needs a favicon
and a header mark, so these ship with the front door.

Five things about the assets that are easy to get wrong:

- **Two forms, different jobs.** `drydock-mark.svg` for in-page use where the ground is known;
  `drydock-badge.svg` (the mark knocked out of a steel rounded square) wherever the surface belongs
  to someone else — favicon, app icon. `drydock-mark-mono.svg` inherits `currentColor` and carries
  **no letters**, because a stencil needs something to be knocked out of.
- **The SVGs and the PNGs are not generated from one source.** Change a path in `icons/` and you must
  change `render-icons.mjs` too and re-run it; nothing warns you when they drift. It takes a JSON
  argv of `[path, size, mode, colors]` entries and uses only Node builtins:
  ```sh
  node docs/design/brand/render-icons.mjs '[["docs/design/brand/icons/favicon-16.png",16,"badge","steel"],
    ["docs/design/brand/icons/favicon-32.png",32,"badge","steel"],
    ["docs/design/brand/icons/apple-touch-icon-180.png",180,"bleed","steel"],
    ["docs/design/brand/icons/github-app-200.png",200,"bleed","steel-lift"],
    ["docs/design/brand/icons/github-app-dev-200.png",200,"bleed","steel-inverse"]]'
  ```
  As of this writing the committed PNGs match the renderer byte-for-byte.
- **Re-render `icon-preview.html` after any geometry change.** It is what caught every measured
  problem in the note — the badge ground is steel because ink navy scores **1.05** against a dark
  page, and the badge is a single knockout because keeping the accent container scores **1.00**
  between the letters and their ground.
- **The badge ground is deliberately not a brand colour.** Steel leaves `#1d4ed8` meaning *API
  traffic* and `#6d28d9` meaning *preview origin* in the diagrams, rather than making a brand colour
  and a semantic colour the same colour.
- **The GitHub App logos are opaque, full-bleed PNGs, and the two Apps differ by inversion.**
  `github-app-200.png` (prod) uses the lighter steel `#64748b`, not the badge's `#475569`. An
  uploaded PNG cannot follow GitHub's theme, and `#475569` scores 2.50 against GitHub's dark page.
  `github-app-dev-200.png` is the same image inverted: a swap of light and dark rather than a new
  hue, because every hue is already semantic in the diagrams. Both come from `render-icons.mjs`
  (mode `bleed`, palettes `steel-lift` and `steel-inverse`). They are uploaded to each App's
  settings by hand.

Still open: a wordmark. The stencilled `DD` is gone by 20 px, so the name is only unambiguous in a
lockup, and the UI header needs one regardless.

## Docs

Design docs live under `docs/design/<scope>/`, with diagrams in a sibling `diagrams/` directory —
except `brand/`, whose assets are products rather than illustrations and live in `icons/`.
Spike results live under `docs/design/spikes/`, each one a numbered report next to the re-runnable
harness that produced it — a spike whose evidence cannot be re-checked against a new Claude Code
version is worth very little. Spike 00's harness is `harness/`; later ones are `harness-NN-<topic>/`,
each with a README naming the command per result. Keep that pairing: the report cites the mode that
produced each number, so a finding can be re-measured rather than re-argued.

Diagrams ship as light/dark SVG pairs (`NN-name-light.svg` / `NN-name-dark.svg`) referenced from a
`<picture>` element with a `prefers-color-scheme: dark` source, and every one carries a descriptive
`alt` and a `**Fig N** —` caption explaining what the reader should take from it. Match that pattern
when adding diagrams.
