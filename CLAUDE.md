# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

**Mostly design, with the first code in.** The repository contains the overall design
document (`docs/design/overall/drydock-design.md`, draft v23), supplemental ones on port forwarding
(`docs/design/port-forwarding/`, draft v5), testing (`docs/design/testing/`, draft v11) and the Vue
frontend (`docs/design/frontend/`, draft v5), a settled brand mark (`docs/design/brand/`, v1.1,
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
go build -o drydock ./cmd/drydock                 # the one binary
printf "%s\n" "$PW" | ./drydock passwd --db x.db  # set the operator password (no HTTP route can)
./drydock serve --db x.db --ui-origin https://drydock.example.com --ui-host drydock.example.com \
  --socket-group drydock                          # see `./drydock serve -h` for the rest
```

| Package | Is |
|---|---|
| `internal/api` | The route table as **data**, both muxes, the gate interface, the error envelope — and ten meta-tests that walk the table. |
| `internal/sys` | `Clock`, `DiskUsage`, `Random`. Never call `time.Now()` directly. |
| `internal/subproc` | An invocation described as data, resolved by `PATH` or an injected `Resolver`, and `Exec`, the real runner: no shell anywhere, `Env` replaces rather than inherits, a non-zero exit is data, and cancelling sends `SIGTERM`. |
| `internal/config` | Settings that must not be constants, `LabelPrefix` chief among them, plus a `Validate` that refuses configurations which silently undo a design property. |
| `internal/store` | SQLite in WAL mode, the single-instance lock, and §4's schema with its enumerations as `CHECK` constraints. A golden snapshot pins the schema. |
| `internal/classify` | The five classifiers, implemented and tested against the corpus: login, identity, refusal, discovery, container. Built in parallel by four agents, one file each. |
| `internal/auth` | argon2id with a floor and rehash-on-sign-in, sessions stored only as SHA-256, and a lockout that is per-IP backoff plus a global cap, kept in `auth_attempt` so a restart does not reset it. |
| `internal/events` | The append-only event log and its live fan-out. Append writes and publishes under one lock so subscribers see id order; a subscriber that lags 256 events is cut off rather than allowed to block writers, and recovers by replay. `data` is a JSON object for the reducer; `message` is prose nothing may parse. |
| `internal/workspace` | The workspace state machine — a transition table where `deleting` is a sink and nothing reaches `running` except from a build — and design §6's eight steps, each writing `workspace.step` started/done/failed so a failure names its step. A step's raw error never reaches an event (a subprocess's stderr can carry anything, git's quoting the URL); only a `workspace.Public` sentence does. Creates check the duplicate and the cap inside one transaction, which is why the store opens every transaction `IMMEDIATE`. `Remove` (only from `deleting`) takes the `supervisor` row with it and keeps the event log, `token_grant` and `secret_access` — the "which workspaces ever held this secret?" history outlives the workspace. |
| `internal/clone` | Design §6 step 2, the host clone. A `contents:read` token for the one repository reaches git only through its environment: a `GIT_CONFIG_COUNT` credential helper that prints it from an environment variable. It is never in argv, a URL or `.git/config`. git runs with global and system config at `/dev/null` and `GIT_CEILING_DIRECTORIES` above the workspace, so an enclosing repo's `http.extraheader` cannot win. It never deletes a directory already at the clone path. Tested with a `GIT_TRACE` wrapper that records every child process's argv, plus a canary sweep of the tree and the database. |
| `internal/container` | `devcontainer up` (argv built and validated from workspace data, result read by `classify.ClassifyContainer`) and finding containers by label: `docker ps -q --filter label=…` for ids, `docker inspect` for structured labels and state. Never a table parse. The id-labels carry workspace id, repository id, repo and branch, so a row can be rebuilt from them. `read-configuration` is parsed too (one JSON object; an unparseable `devcontainer.json` exits `0`, so "names no image" is checked here). A repository's committed `devcontainer-lock.json` is **honoured** (`LockfileHonour`: no lockfile flag) and one without gets `--no-lockfile`, so none is created — never `--frozen-lockfile`, which refuses a lockfile a commit stale that VS Code would quietly rewrite. With no flag `up` leaves an in-sync lockfile byte for byte and rewrites a stale one to VS Code's bytes, **but only because Drydock's Feature has no `dependsOn`**: the CLI writes an injected Feature's dependencies into the lockfile (design §6, "The repository's lockfile"; the measured matrix, with Drydock's real Feature across create, start and rebuild, is `test/fixtures/devcontainer/lockfile-behaviour.txt`). A lockfile with an entry for Drydock's own Feature, or one that is not a regular file inside the clone or does not parse, is refused. Each `up` gets its own `TMPDIR`: the CLI stages Features in a folder named by the millisecond, and concurrent creates shared one. `--override-config` goes to `exec` as well as `up` — `exec` reads the config too. `Find`/`Stop`/`Remove` act on the containers carrying one workspace's label (never a cached id); every id handed to `docker stop`/`rm` must be a full 64-hex id, after `--`; `rm` is `--force --volumes`, which takes anonymous volumes and never named ones. |
| `internal/provision` | Design §6 end to end, behind `POST /api/workspaces` and `/start`: the eight real step functions, run off the request path, one run per workspace, bounded by a timeout and by shutdown (each fails the interrupted step with a sentence saying which). An honoured lockfile is never repaired: after `up` its bytes are compared with what was there before (bytes, not `git status` — git on the host would run whatever the container-writable `.git/config` names), and a stale one `up` rewrote stays in the clone with the `up` step's detail naming the file, since it is exactly what VS Code would write and the right thing to commit. A repository with no `devcontainer.json` — decided by looking for the file, since `read-configuration` fails silently — gets `mcr.microsoft.com/devcontainers/base:debian` written *beside* the clone. Steps 4 and 8 are recorded no-ops (`workspace.Note`) until Phase 5. `Owns` is reconciliation's `Busy`, so a create in the first seconds after boot is not marked "interrupted". One workspace per repository in any state; a start honours the cap. Tested with a fake CLI for argv and every step's failure sentence, and in `test/container` for real. Phase 6's `Stop`, `Rebuild` and `Delete` (`lifecycle.go`) are jobs under the same one-per-workspace ownership, each sub-step writing `workspace.action`: a stop is refused unless `running` and closes the broker socket; a rebuild (and a start from `failed`) passes `--remove-existing-container`; a delete is persisted as `deleting` first, cancels and waits for a run in flight, and removes only `<WorkspaceRoot>/<ULID>` (clone, `.drydock/` and its per-`up` `tmp/`) proved to be a real directory matching the row (never following a symlink). `StopSupervisor` is Phase 5's seam, stopped *before* the container. `ResumeDelete` is reconciliation's. Crash-tested by cutting a delete off after every sub-step and resuming in a fresh process. |
| `internal/reconcile` | Boot reconciliation (§6) as a pure `Plan(rows, found)` plus `Run`. Adopt rather than kill, never auto-start, and a failed `docker` listing changes nothing. Wired into `Serve` beside serving; without Docker access it writes one warning and carries on. A `deleting` row is finished by `provision.ResumeDelete` — the route's own delete — and broker sockets are reopened after it, for `running` workspaces only. |
| `internal/github` | The App client: the key loaded from a file (refused if its group or others can read it), an RS256 JWT built on the standard library, installation tokens cached in memory for 55 minutes by what they grant, and paginated listing. A `Token` formats as `[redacted]` and refuses to marshal, and a token request must name its permissions. `githubtest` is the fake GitHub: it verifies the JWT and enforces each token's permissions and repositories, and `EnableGit` adds a git smart-HTTP remote (git's own `http-backend` behind GitHub's token authorization) that records every token it is shown. |
| `internal/catalog` | The repository list: refreshed at boot, every 15 minutes and on demand, with concurrent refreshes joined. A listing token asks for metadata only and a probe token for contents read. A repo is probed for `devcontainer.json` only when it was pushed to, and a failed probe is `null`, never `false`. A repo dropped from the installation is deleted unless a workspace holds it, and its secret grants with it. |
| `internal/broker` | The token broker (§9). Each workspace gets a socket, `0666` in a `0700` directory, and the socket a connection arrives on decides the workspace. The line protocol is `GET-TOKEN scope=git` or `scope=gh`, plus `PING`, parsed strictly: an extra field is `bad_request`. Each scope asks for exactly §9.3's permissions (golden files in `testdata/`), for exactly the workspace's one repository. The repository's state is read per request, a GitHub refusal never falls back to anything broader, and `token_grant` and `token.issued` are written per mint, not per cache hit. `GET-SECRETS` (no arguments, ever) answers `OK count=N`, N `NAME value` lines, `END` from the secrets store's snapshot — no GitHub request, no decryption — and writes `secret_access` before answering, or does not answer. `Close` also removes a stale socket file an earlier process left, so a delete resumed at boot leaves no socket. |
| `internal/secrets` | Repository secrets (§10). XChaCha20-Poly1305 under a 32-byte master key read once from a `0400` file (`Key` formats as `[redacted]`), a fresh nonce per write, the secret's id as AAD. Validation on write: the name pattern, the reserved list (exact names plus the `CLAUDE_`, `ANTHROPIC_`, `DISABLE_`, `GH_`, `GIT_`, `DRYDOCK_`, `LD_`, `BASH_` prefixes — each with its reason, which the refusal quotes), any Unicode control character or non-UTF-8 in a value, an empty or over-32-KiB value, a blank reach. Default deny; `all_repos` widens to every repository. `Resolve` serves the broker from a snapshot decrypted once per write, and refuses *everything* if any row is undeliverable. `Put` answers the stale running workspaces split by `StaleKind` (a hook; nil means `new_commands`, true until Phase 5). `Meta` has no field for a value. |
| `feature/src/drydock/` | The devcontainer Feature `drydock` (§11), published to `ghcr.io/krelinga/drydock/drydock` by `feature-publish.yml` when its own version changes. It installs the clients into `/usr/local/drydock/bin`, linked from `/usr/local/bin` because a login shell drops `containerEnv`'s `PATH`. It installs `gh` itself (GitHub's apt and dnf repositories, Alpine's `github-cli`; an image's own `gh` is kept; any other image fails the install) and **must never declare `dependsOn`**: the CLI writes an injected Feature's dependencies into every repository's `devcontainer-lock.json` (design §6, §11). It sets system git config (helper, bot identity, `core.hooksPath`) and adds a `pre-push` guard for the `drydock/` prefix that passes every other hook through to the repo's own. `containerEnv` points `CLAUDE_ENV_FILE` at `etc/claude-env.sh`, a shipped file whose whole text is `eval "$(drydock-secrets export || echo exit 69)"`; nothing writes it at install or start. **Bump `version` in `devcontainer-feature.json` with any change here**, or it is not republished. Tested by `devcontainer features test -p feature --skip-autogenerated` in CI's `feature` job, against a stand-in broker. |
| `feature/src/drydock/bin/` | The in-container clients, POSIX `sh`: `drydock-broker` (the transport, over `socat` or `nc -U`), `drydock-credential` (git's helper, for GitHub's host only) the `gh` shim (fetches a token per call and execs the real `gh` with it in the environment, not argv), and `drydock-secrets export` (single-quotes every value, `'` as `'\''`; fails the fetch unless `count=`, every line's shape and `END` agree; silent on success; on failure one stderr line **and `exit 69` on stdout**, because `eval "$(…)"` discards the helper's own status). They are tested from Go against a real broker socket over both transports, and with real git against the fake's git remote. |
| `internal/server` | Assembles the front door: store, auth, both muxes, two `0660` group-owned sockets, and no TCP listener — asserted on the running process. |
| `cmd/drydock` | `serve`, `passwd` and `version`, and nothing that binds TCP or sets a password over HTTP. `passwd` deliberately skips the instance lock so it works while the server runs; `--if-unset` makes it a no-op that never reads stdin once a password exists, which is what keeps an installer re-run from signing everyone out. `version` is stamped by `-ldflags -X main.version=`. `serve --secrets-key` takes the master key's *path*. |
| `web/src/stores` | `reducer.ts` is the one pure writer of entity state. It applies events, `resync`, and three snapshots — `GET /api/repos`, `GET /api/workspaces`, `GET /api/workspaces/:id` — each tagged with the stream position it was requested at. Each field carries the id of the event that last wrote it, so a replay changes nothing and a late event is a no-op. **Only the workspace list may drop a workspace**: the catalog joins just each repo's newest, so its silence proves nothing. Each workspace has a step timeline versioned per step, and a feed of its last 50 events merged by id. `workspaces.ts` holds the fetches and the two Phase 2 mutations; a clone is settled by the create's `workspace.state` carrying its `repository_id`, because the only workspace id before that is in the `202` body, which is never read. `stream.ts` owns the `EventSource`, the in-flight set and the refetch hooks, and handles reconnects: a quiet marker after 5 s, and on `CLOSED` a session probe, then sign-out or a hard retry that resumes with `?last_event_id=`. Specs drive `test/fakeEventSource.ts`, because jsdom has no `EventSource`. |
| `web/src/views`, `components`, `lib` | Home (`Running` + the catalog with a Clone or Start per row) and `/ws/:id` (state, the eight-step timeline, the feed as text, Start). `components/ActionButton.vue` is §4.2 for every mutating button: in flight until the settling event, *"no response yet"* after 10 s and never a failure, refusals by code, `in_progress` a note. `lib/workspaceCard.ts` is §6.1's table as a pure function; only the workspace half exists, and `supervisorHalf()` is a marked Phase 5 seam that must not be filled from `workspace.state`. `mocks/backend.ts` serves the workspace routes with the cap and the duplicate check; `scriptMode: 'manual'` holds a create's events for a spec to play with `playScript`. |
| `internal/web`, `web/` | The Vue 3 app in `web/`, embedded from `internal/web/dist`, which is **committed build output** — the Go build needs no Node, and `npm run check:dist` fails when it is stale. Never hand-edit `dist`; rebuild it. |
| `web/src/views/secrets/`, `web/src/lib/secretRules.ts` | Phase 4's UI, one lazy chunk: the list, the write-only form (`reach` labelled with the literal question), grants with the `all_repos` confirm, the two-kinds rotate result, delete. Secret entities are written only by `GET /api/secrets` and `secret.*` events. The `PUT`'s 200 body is used for its `stale` lists and never applied. The value lives in one component `ref`, in a `<textarea>` (an `<input>` strips a pasted newline) inside no `<form>`, and a canary spec proves it is nowhere after submit. `secretRules.ts` mirrors `internal/secrets/validate.go`, and its spec reads that file, so **change a reserved name or a limit in Go and that spec fails until the copy matches**. |
| `deploy/Caddyfile`, `deploy/preview.caddy` | The entire LAN-facing surface, every value an env placeholder so the shipped files are the tested files. The preview site is a separate, optional file imported by glob, so a first deployment needs no wildcard certificate. |
| `deploy/install.sh` | The installer and upgrader, one file in two modes: standalone (`curl … \| sudo bash`) it downloads and verifies the release tarball and runs the copy inside; from the tarball it installs. Settings persist in `/etc/drydock/drydock.env`, parsed, never `source`d. Idempotent: files are written only when they change, and only what changed is restarted. It requires Docker and the devcontainer CLI (Node 20+) on the service's `PATH` and installs neither; it adds `drydock` to the `docker` group — root by another name, design §13.4 — and gives the unit `ReadWritePaths=/srv/drydock/ws`. The first install creates the secrets master key, `/etc/drydock/secrets.key` (32 bytes of `/dev/urandom`, `0400`, `drydock`'s, never printed); later runs keep it and **refuse rather than replace** one that is not a 32-byte file. A rollback restores the previous unit with the previous binary. |
| `deploy/package.sh` | Builds the release assets — the same script in CI and in the installer test. |
| `test/install/` | `run.sh` runs the installer against real systemd, the official Caddy package, Debian's Docker (a nested daemon) and the devcontainer CLI in a privileged container: refusal without Docker or the CLI, a real `devcontainer up` inside the running service's own mount namespace, fresh install, no-op re-run, upgrade, rollback, previews on and off, an unreadable key, a foreign Caddyfile, the secrets master key (created, never printed, kept across re-run and upgrade, a damaged one refused). `live.sh vX.Y.Z` runs the README one-liner against a *published* release from GitHub. Neither is part of `go test`; CI runs the first, the release workflow the second. |
| `test/container/` | The container tier: real Docker (the devcontainer's DinD, or the CI runner's) and the real `devcontainer` CLI. Adopt-an-orphan and died-unobserved against real containers, and Phase 3's deliverable end to end. That is a real `devcontainer up` with the Feature from this checkout and the broker socket bind-mounted. Inside, git pushes a `drydock/` branch through the helper; a push to `main` is refused, the other repository is unreachable, and there is no socket but its own and no Docker socket. Then Phase 4's: a suite run through the Feature's `CLAUDE_ENV_FILE` passes while a secret is granted and fails on the next command once it is not; a hostile value runs nothing; no value is in any `/proc/*/cmdline` or `docker inspect`. And Phase 2's: `POST /api/workspaces` through the real server, signed in, for a repository with a config and one without, both reaching `running` with the published Feature, the clones untouched and no token anywhere in the tree or the database; a secret granted to one reaches its `CLAUDE_ENV_FILE` prelude and not the other's. Each test runs under its own random label prefix. Skips without Docker unless `DRYDOCK_REQUIRE_DOCKER` is set, which CI does. |
| `test/component/` | Real binaries, nothing mocked. Today: the Caddyfile conformance test (testing §3.2), mutation-checked against the Caddyfile itself. |
| `test/fixtures/` | The corpus: 40 fixtures from `2.1.289` and devcontainer CLI `0.89.0`, plus `record.sh`, which is testing §11.1 step 3. Some are hand-written or synthetic, and their `.meta` says which. |

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
devcontainer**: `--with-deps` pulls in some forty transitive system libraries, and enumerating those
in a package list is how the list goes stale silently. It stays a lifecycle command —
`npx playwright install --with-deps chromium` — to be added to `postCreate` once `web/package.json`
exists in Phase 1. A `drydock-playwright-cache` volume on `~/.cache/ms-playwright` is already
mounted so that ~114 MB download survives rebuilds, the same reasoning as the DinD volume beside it.
**Both changes need a container rebuild to take effect.**

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

Merging release-please's PR tags `vX.Y.Z` (no component prefix: one root package) and, in the same
workflow, runs the suite and uploads the assets under **fixed names** so
`releases/latest/download/<name>` always resolves: `drydock_linux_amd64.tar.gz` (binary,
`install.sh`, both Caddy files, `VERSION`), `SHA256SUMS`, and `install.sh` stamped with its tag. It
is one workflow, not a tag-triggered second one, because tags pushed with `GITHUB_TOKEN` trigger
nothing. The repo setting *Allow GitHub Actions to create and approve pull requests* must be on.

After the upload, a `verify` job runs `test/install/live.sh <tag>` — the README line against what was
just published. A red `verify` means a broken release is already public: fix forward with a `fix:`.

**CI** (`.github/workflows/ci.yml`) runs on every PR and push to `main`: `gofmt`, `go vet`, `go test`
with Caddy installed and `DRYDOCK_REQUIRE_CADDY=1` — without it a missing `caddy` is a *skip*, which
in CI is a silent pass of the Caddyfile test — then `npm run check`, then `test/install/run.sh`. Its
`CADDY_VERSION` is pinned; the devcontainer's Caddy feature is not, so bump the pin when a rebuild
moves it. Releases are amd64 only, by choice.

**The GitHub contract tests** (`.github/workflows/github-live.yml`) run the `TestContract*` functions
against the real dev App, `krelinga-drydock-dev` (App ID 5189839), which is installed on
`krelinga/drydock-testbed-a` (declares a dev container) and `-b` (does not) and nothing else. The same
functions run against `githubtest`'s fake on every `go test`, through `githubtest.NewBackend`. So when
the live run fails and the fake run passes, the fake holds a belief about GitHub that is wrong, and
the failing test names it. The job runs on PRs, on `main`, nightly and on demand. It needs the
repository secret `DRYDOCK_DEV_APP_KEY` (the `.pem`) and the variable `DRYDOCK_DEV_APP_ID`.
`DRYDOCK_REQUIRE_GITHUB_LIVE` stops it quietly testing the fake. **The testbed repos are fixtures**:
change one and you must change `githubtest.Testbed()` too. Both must stay private, or the scoping
tests test nothing.

The README's one-liner is the contract: change an asset name, a flag, or `/etc/drydock/drydock.env`
and an existing install's re-run is what breaks. `test/install/run.sh` installs from a local copy of
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
  `Enable Remote Control? (y/n)` prompt rather than failing. The feature writes all three.
- **These variables must stay unset in every container:** `ANTHROPIC_BASE_URL` (or point at
  `api.anthropic.com`), `DISABLE_TELEMETRY`, `DO_NOT_TRACK`,
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `DISABLE_GROWTHBOOK`. Any of them disables Remote
  Control while everything still builds and starts. They are also on the reserved-secret-name list.
- **Auth is middleware around the whole mux**, so a route added later is protected by default. Only
  the sign-in POST is unauthenticated, and it is rate-limited and lockout-guarded.
- **No route returns a secret value** — no reveal button, no edit form, no re-auth escape hatch. The
  schema has no `value` column, and it should stay that way.
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
  in the Feature, in `internal/classify`, and in each spike report. Spike `04` is about browser
  behaviour and has its own trigger (testing §11.6).
- **The shared credential volume must be a local Docker volume — never NFS or CIFS.** Claude Code's
  cross-container refresh lock is a `mkdir(2)`-based lockfile at
  `$CLAUDE_CONFIG_DIR/.oauth_refresh.lock`; network filesystems do not give `mkdir` the atomicity the
  whole guarantee rests on (Spike 00).
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
   is built as specified and keeps all nine of its assertions.

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
6. **Livability** — stop/rebuild/delete, concurrency cap, disk and session counts, log viewer.
   **Server side of stop, rebuild and delete done**, with boot reconciliation resuming a delete and a
   container-tier test through provision → stop → start → rebuild → delete. Still open: their
   buttons, disk and memory on the card, and what Phase 5 owns (session count, log viewer).

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
