# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.
It holds what an agent needs before touching anything. Each package's detail lives beside its code:
the Go package comment (`go doc ./internal/<pkg>`) or the directory's `README.md` — **read it before
working in that package, and keep it current when you change the package.**

## Repository state

The design: `docs/design/overall/drydock-design.md`, supplemented by port forwarding
(`docs/design/port-forwarding/`, "PF"), testing (`docs/design/testing/`) and the Vue frontend
(`docs/design/frontend/`); a settled brand mark (`docs/design/brand/`, v1.1, with the shipping icon
assets); an adversarial security review (`docs/design/security-review.md`); and five spikes with
re-runnable harnesses under `docs/design/spikes/`. Read the overall design before making
architectural decisions — most "why is it like this?" questions are answered there. When a change
contradicts it, update the doc in the same change.

The Go module is `github.com/krelinga/drydock`; the UI is Vue 3 + TypeScript + Vite in `web/`.

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

| Package | Is — and what not to break there |
|---|---|
| `internal/api` | The route table as **data**, both muxes, the gate, the error envelope, the preview front door; meta-tests walk the table. |
| `internal/sys` | `Clock`, `DiskUsage`, `Random`, and `Cleanup` (the context rule's rule 3). Never call `time.Now()` directly; `TestContextRule` enforces it. |
| `internal/life` | `Group` (goroutines stopped and waited for as one: nothing starts after `Stop`, `Wait` names stragglers on an injected-clock deadline) and `Coalescer` (periodic + on demand on one worker; a ticket is answered only by a run begun after it, and its payload, if `TriggerWith` gave one, reaches that run alone; `Await` of ticket 0 is `ErrNoTicket`). A `Child` is released once its own `Wait` finds it ended, so a child per job must be waited for. Not exempt from the context rule. |
| `internal/subproc` | Invocations as data, resolved by `PATH` or a `Resolver`. No shell anywhere; `Env` replaces rather than inherits; `StartPTY` is the one PTY start. |
| `internal/config` | Settings that must not be constants (`LabelPrefix` first); `Validate` refuses configs that silently undo a design property (`CrossSite`, lowercase origins). |
| `internal/store` | SQLite in WAL mode, the single-instance lock, §4's schema with enums as `CHECK`s. Every transaction opens `IMMEDIATE`; a golden snapshot pins the schema. |
| `internal/classify` | The five classifiers over the recorded corpus. They refuse rather than guess. |
| `internal/claudetest`, `internal/pty` | `fakeclaude`, replaying the corpus on a real PTY, its replays pinned by SHA-256; and the PTY itself. |
| `internal/auth` | argon2id, sessions stored only as SHA-256, a lockout that survives restart. |
| `internal/preview` | Preview tokens (memory only, single-use), preview sessions (stored as hashes), the preview proxy and the port registry. `forwarded_port` has no foreign key to `workspace`: a slug is never reissued. A port is off until enabled and nothing a container declares enables it; a disable or retire closes its websockets at once; the probe is the proxy's own dial. |
| `internal/events` | The append-only event log and its fan-out. `Commit` makes a row change and its event one fact; `message` is prose nothing may parse. |
| `internal/workspace` | The state machine and §6's eight steps. A step's raw error never reaches an event; the cap's rule is `Occupying` and nothing else; the host-access approval state. |
| `internal/clone` | §6 step 2. The token reaches git only through its environment — never argv, a URL or `.git/config`. |
| `internal/container` | `devcontainer up`/`exec`/`read-configuration`, finding containers by label (never a table parse), the host-access subset, the cleanup and volume-owner helpers. Every devcontainer invocation runs docker through the guard. |
| `internal/dockerguard` | The docker guard: the drydock binary run as `docker`, holding every container-creating command to the approval. An allowlist that fails closed. |
| `internal/provision` | §6 end to end, and stop/rebuild/delete, as jobs, one per workspace, each a goroutine of `Serve`'s `life.Group` (`RunIn`): admitted under `p.mu` before anything is written for it, so a job asked for once the group stops is `ErrShuttingDown` (503) with nothing written, and shutdown waits for every cut-off job's step failure and `workspace.job` end before the database closes. Bounds are on the injected clock. A delete removes only `<WorkspaceRoot>/<ULID>` proved real, never following a symlink. |
| `internal/supervisor` | §8: one `claude remote-control` per running workspace. Exits classified by message, never status; stops signal inside the container; the log ring owns its redaction. |
| `internal/redact` | The one masking rule for subprocess text Drydock shows. |
| `internal/usage` | Memory and disk for the card. A measurement is never an event; unknown is never zero. |
| `internal/reconcile` | Boot reconciliation: a pure `Plan` plus `Run`. Adopt, never kill; never auto-start; a failed listing changes nothing. |
| `internal/github` | The App client, and `githubtest`, the fake GitHub the contract tests also run against. A `Token` formats as `[redacted]` and refuses to marshal. |
| `internal/catalog` | The repository list, refreshed by a `life.Coalescer` under `Serve`'s `life.Group`, stopped and waited for before the database closes. Grants deleted outside `internal/secrets` must call `secrets.Store.Invalidate`. |
| `internal/identity` | The expiry watch (§7.3), its checks on a `life.Coalescer` under `Serve`'s `life.Group`, never under a caller's context (`LoggedIn` is a request carrying the handshake's moment). The credential's bytes reach nothing but the classifier; every check ends, and every requested check is answered. |
| `internal/claudeimage` | The one image Drydock runs Claude Code in itself, at exactly `classify.ClaudeCodeVersion`. |
| `internal/login` | The login handshake (§7.2) on a PTY Drydock owns, each login's session and helpers in `Serve`'s `life.Group` (shutdown is one more end, waited for before the database closes). The code is never stored or copied into a string; every end removes the container by label first. |
| `internal/broker` | The token broker (§9). The socket's directory is what a container mounts and must outlive the process; a refusal never falls back to anything broader. |
| `internal/secrets` | Repository secrets (§10): sealed, validated on write, default deny. Any undeliverable row refuses everything. |
| `internal/server` | Assembles the front door: two `0660` sockets and no TCP listener, asserted on the running process. |
| `internal/web` | Serves the embedded UI from `internal/web/dist`, which is **committed build output**: never hand-edit it; rebuild. |
| `cmd/drydock` | `serve`, `passwd`, `count-secrets`, `check-preview-domain`, `version` — and the docker guard when run as `docker`. Nothing binds TCP or sets a password over HTTP. |
| `web/` ([README](web/README.md)) | The Vue app. The reducer is the one writer of entity state; an in-flight mark ends on the settling event, never the `202`. |
| `feature/` ([README](feature/README.md)) | The devcontainer Feature and its in-container clients. **Bump its `version` with any change**, and never declare `dependsOn`. |
| `deploy/` ([README](deploy/README.md)) | Caddyfiles and the installer. Everything that can refuse runs before anything is replaced. |
| `test/install`, `test/container`, `test/browser`, `test/ansible`, `test/docs`, `test/component` | Each has a README naming what it covers. |
| `test/fixtures/` ([README](test/fixtures/README.md)) | The corpus from Claude Code `2.1.289` and devcontainer CLI `0.89.0`, plus `record.sh` (testing §11.1 step 3). |

Three things about that code worth knowing before extending it:

- **Handlers are `nil` for routes a later phase owns**, and `Build` mounts a `501`. That is deliberate:
  the table is the complete contract, reviewable against §5 as a list, and the gate is provably
  applied to routes nobody has written yet. **The auth gate runs before that `501`** — otherwise an
  unauthenticated caller could tell a declared route from a nonexistent one and read the API surface
  off a server it cannot use. There is a test for the ordering; do not reorder it.
- **Classifiers refuse rather than guess.** A classifier that quietly returns `IdentityOK` for bytes it
  cannot parse is exactly the failure the corpus exists to prevent, so each returns an error for input
  it cannot read — an unreadable credential file is never `absent`, a URL fragment is never usable.
  The identity classifier decides `blanked` from the file *before* reading `auth status`, so the less
  stable input can never hide the fleet-wide failure.
- **The meta-tests were mutation-checked**, not just observed passing (moving the `501` ahead of the
  auth gate, marking a route unauthenticated, deleting the `Origin` check each fail them by name).
  Keep that property — a negative test nobody has watched fail might be vacuous (testing §4.1).

Two docs ship something runnable; open them before arguing the subject in prose:
`docs/design/frontend/prototype/prototype.html` (every non-trivial UI surface at 360 px) and
`docs/design/brand/icon-preview.html` (every icon form, with measured contrast).

### The devcontainer

`.devcontainer/devcontainer.json` carries the toolchain: Go (with golangci-lint), Node,
**docker-in-docker**, the `devcontainer` CLI, Caddy, `gh`, `libnss3-tools`, and
`sqlite3`/`socat`/`nc`/`jq`.

- **Docker-in-docker, deliberately not DooD.** Drydock hands the daemon host paths to bind-mount (the
  clone, the broker directory); under DooD the daemon is the host's, those paths resolve differently,
  and every mount breaks — the same path-identity problem §1 gives for not containerizing Drydock. The
  inner `/var/lib/docker` is a named volume (run only one such container at a time), and containers
  Drydock creates are invisible to the host's `docker ps`.
- **`devcontainer-lock.json` is generated — never hand-edit it.** Its digests are trusted *input*
  during resolution, so a wrong hand-written digest silently installs content the tag no longer points
  at. Adding a feature means editing `devcontainer.json` only.
- **Playwright's browsers are not installed by features** (`--with-deps` pulls ~40 system libraries,
  and a package list of them goes stale silently). `postCreateCommand` runs `npm ci` in `web/` then
  `npx playwright install --with-deps chromium firefox webkit`, cached in the
  `drydock-playwright-cache` volume. A changed `postCreate` needs a rebuild; until then run the two
  commands by hand in `web/` (system libraries: `sudo npx playwright install-deps chromium firefox
  webkit`). `certutil` (`libnss3-tools`) is how the browser tier's test CA gets into Chromium's trust
  store — the only route that keeps certificate validation on.
- `@playwright/test` is pinned in `web/`; bump it with the §11.6 ritual (a new Chromium is a new set of
  cookie rules), and move CI's `mcr.microsoft.com/playwright:v<version>-noble@sha256:…` tag and digest
  with it — `test/docs` fails while they disagree or the image is not pinned by digest.
- `drydock-gh-config` keeps `gh`'s login across rebuilds. Log in with a **fine-grained token scoped to
  this repository alone** (`! gh auth login --with-token`), not the default OAuth login; `hosts.yml`
  holds it in plain text in a `0700` directory.

## Releases and CI

**Commit subjects are conventional commits, and that is load-bearing.** release-please reads them off
`main` (PRs are squash-merged, so it is the PR title, which `pr-title.yml` checks): `feat:` and `fix:`
cut a release, `feat!:` a breaking one, and `docs:`, `test:`, `chore:`, `ci:`, `refactor:` do not. A
design-doc change is `docs:` even when it is large.

**The release flow.** Merging release-please's PR tags `vX.Y.Z` (one root package, no component
prefix) and creates the release **as a draft** (`"draft"` + `"force-tag-creation"`: GitHub tags a draft
only on publish, and release-please finds its previous release by the tag's commit). In the same
workflow — one workflow, because tags pushed with `GITHUB_TOKEN` trigger nothing — `test` runs the Go
suite, `assets` uploads under **fixed names** (`drydock_linux_amd64.tar.gz`, `SHA256SUMS`, a stamped
`install.sh`) so `releases/latest/download/<name>` always resolves, `verify` downloads the draft's
assets back and runs `test/install/live.sh --dir` (fresh host and upgrade of the current *Latest*),
and only then `publish` makes it public and *Latest*, returning it to a draft if
`releases/latest/download` does not serve it. A red job leaves a draft and the previous release
*Latest*. `verify` and `test` take `test/install` and `.github/actions` from the workflow's own commit.

- Re-run failed jobs (`gh run rerun --failed`) only for a transient failure: a re-run uses the
  workflow file its run started with.
- When the fix is to the workflow or its test environment (`.github/`, `test/install/`), merge it as
  `ci:` and resume the draft: `gh workflow run release-please.yml --ref main -f tag=vX.Y.Z`. Fix
  forward with a `fix:` when the code is wrong.
- The release PR gets no CI (a `GITHUB_TOKEN`-opened PR triggers no workflows), so it merges only with
  an admin bypass of the ruleset — the owner's decision. Agents never merge with `--admin`.
- The repo setting *Allow GitHub Actions to create and approve pull requests* must be on.

**CI** (`.github/workflows/ci.yml`) runs on every PR and push to `main`: `gofmt`, `go vet`, `go test`
with Caddy, `socat`, `nc` and the devcontainer CLI and `DRYDOCK_REQUIRE_CADDY=1`,
`DRYDOCK_REQUIRE_DOCKER=1`, `DRYDOCK_REQUIRE_TRANSPORTS=1` (without them a missing tool is a *skip*,
which in CI is a silent pass); `npm run check`; `test/install/run.sh`; the Feature tests; and the
`browser` job.

- **The Go suite's tools and commands live in one composite action, `.github/actions/go-suite`**,
  run by CI's `go` job and the release's `test` job, so a release is held to exactly what every PR
  passed (two copies once drifted and broke a release). Add a test dependency there, never to one
  workflow. It is a composite action, not a reusable workflow, because the ruleset requires a check
  named `go`. Its Caddy pin (`go-suite/install-caddy.sh`) is the one pin; the devcontainer's Caddy
  feature is not pinned, so bump it when a rebuild moves it.
- The `go` job checks out with `fetch-depth: 0` and passes the PR title, because **a deploy document
  may name only a released version (a `CHANGELOG.md` heading) or the one release-please will cut
  next**. Pin docs to the release your PR will cut; use `vX.Y.Z` for an example.
- The `browser` job runs `test/browser/run.sh` **inside Playwright's own image**, pinned to `web/`'s
  `@playwright/test` version and by digest (apt on the bare runner took up to 42 minutes). It adds Go,
  the pinned Caddy and `certutil` (one `.deb` pinned by URL and SHA-256, checked every run before
  `dpkg -i`), and runs the tier as `pwuser` via `setpriv`, never as root: Drydock refuses to run the
  login as root.
- Releases are amd64 only, by choice.

**The GitHub contract tests** (`.github/workflows/github-live.yml`) run the `TestContract*` functions
against the real dev App `krelinga-drydock-dev` (App ID 5189839), installed on
`krelinga/drydock-testbed-a` (declares a dev container) and `-b` (does not) and nothing else; the same
functions run against `githubtest`'s fake on every `go test`. When live fails and fake passes, the
fake's belief about GitHub is wrong. The job needs the secret `DRYDOCK_DEV_APP_KEY` and the variable
`DRYDOCK_DEV_APP_ID`; `DRYDOCK_REQUIRE_GITHUB_LIVE` stops it quietly testing the fake. **The testbed
repos are fixtures**: change one and change `githubtest.Testbed()` too; both must stay private, or the
scoping tests test nothing. So are the dev App's permissions (`githubtest.DevAppPermissions()`,
asserted against `GET /app`): production's set, `actions: write` included; it must keep lacking
`administration: write`, which pins a missing permission live.

**The deploy documents.** The README's one-liner is the contract: change an asset name, a flag, or
`/etc/drydock/drydock.env` and an existing install's re-run is what breaks.
`docs/deploy/first-deployment.md` is the operator's runbook, every command, path and message taken from
`deploy/install.sh` and the server as built, so **a change to either — a flag, a file the installer
writes, a journal line, a UI label it tells the operator to press — updates it in the same change**,
and `docs/deploy/first-deployment-ansible.md` too (its tasks pass the installer's flags, use `drydock
passwd --if-unset`, and read `changed` off the installer's `==> ` lines; `test/ansible/` checks it).
The runbook's *Known issues* are installer gaps; fixing one means deleting its entry.

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
- **Caddy's admin API is never left on `localhost:2019`.** Any local process can reach that loopback
  port, and whatever can reconfigure Caddy can point it at Drydock on its own terms, undoing the
  argument for the socket. `deploy/Caddyfile` puts it on a `0600` Unix socket; the conformance test
  asserts nothing listens on 2019.
- **Remote Control needs a real `claude auth login` credential**, not `CLAUDE_CODE_OAUTH_TOKEN` (a
  setup token can only make model requests). This is why the PTY login handshake (§7.2) exists.
- **`.credentials.json` alone is not enough.** Remote Control also needs the `oauthAccount` record
  from `.claude.json` in the same directory — without `organizationUuid` it refuses to start though the
  token works for model requests (Spike 02). Sharing the whole `CLAUDE_CONFIG_DIR` carries it; copying
  the credential file does not, which is why that option is struck. Two more keys gate a headless
  start: `hasTrustDialogAccepted` for the workspace path, and `remoteDialogSeen` — without it the
  server *hangs* on `Enable Remote Control? (y/n)`. The Feature writes those two; `oauthAccount` comes
  only from a login, and the Feature never writes it.
- **These variables must stay unset in every container:** `ANTHROPIC_BASE_URL` (or point at
  `api.anthropic.com`), `DISABLE_TELEMETRY`, `DO_NOT_TRACK`,
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `DISABLE_GROWTHBOOK`. Any of them disables Remote
  Control while everything still builds and starts. They are also reserved secret names.
- **Auth is middleware around the whole mux**, so a route added later is protected by default. Only
  the sign-in POST is unauthenticated, and it is rate-limited and lockout-guarded.
- **The `Origin` check is exact-match and fails closed** (§13.5). Every mutating route compares
  `Origin` to the literal UI origin — never a suffix, never a registrable-domain match, never
  case-folded — and refuses one absent, empty, `null`, or sent twice as hard as a wrong one. No
  permissive or `Origin`-reflecting CORS. It is the belt behind `SameSite=Lax`, which does the primary
  CSRF work only because previews are on a *different registrable domain* (`config.CrossSite`); a
  same-site preview domain is refused at startup and by the installer, since its requests would carry
  the cookie.
- **Nothing returns or pre-fills a stored secret value** — no route, response, form or event, no
  reveal button, no re-auth escape hatch. Metadata (reach, description) can be edited, and a *new*
  value written through a field that opens empty; the stored value never travels back out. The schema
  has no `value` column; keep it that way.
- **A secret value is single-line on write and single-quoted on delivery.** The API refuses control
  characters (§10.1); `drydock-secrets export` quotes every value, `'` as `'\''` (§10.3). A newline
  forges a line in the `GET-SECRETS` response and bypasses the reserved-name list; an unquoted value is
  shell code run before *every* Bash command. The client also fails the fetch when `count=` disagrees
  with the lines received.
- **The workspace label prefix is configuration, recorded at first run, and reconciliation refuses a
  foreign one.** Adoption and deletion are label-driven, so a second instance sharing the prefix
  inherits the first's containers *including its delete path*. The SQLite advisory lock does not cover
  this — the second instance has its own database. This is what a test run is.
- **Store no credential but the App private key.** GitHub tokens live in a bounded in-memory cache;
  `token_grant` records that a token was issued, never the token. `auth_session.id` is the SHA-256 of
  the cookie value, so a stolen DB yields no usable cookie. Secret values are XChaCha20-Poly1305
  ciphertext with the secret id as AAD.
- **The App key and the secrets master key never enter the environment** — `0400` files read once at
  startup. Environment variables leak into `/proc`, crash reports, and every child process.
- **No host-affecting setting runs without an operator approval that covers it.** The clone is
  bind-mounted read-write, so the `devcontainer.json` a start or rebuild reads may be the container's
  work, and `up` runs `initializeCommand` on the host as `drydock` (who can read both keys and is in
  `docker`) and hands `runArgs`, `mounts`, `privileged` and the rest to the host's daemon. Step 3
  stops the workspace's containers (so nothing rewrites the file between check and `up`), reads the
  resolved **and merged** configuration (Features and image metadata can ask for `privileged` and
  mounts too), computes the host-access subset by design §6's allowlist — so a field a new CLI adds is
  in it by default — and runs a non-empty subset only if every entry is within the repository's
  current approved set (`container.Covered`: the same value, or for `capAdd`/`securityOpt`/`mounts`
  fewer elements — never `runArgs`, which is argv); running less never narrows the approval. Otherwise
  the run stops `needs_approval`, before any `up`. `read-configuration` was measured not to run
  `initializeCommand`. Approval is not containment: an approved `privileged` (docker-in-docker) is host
  root; the gate guarantees only that a container cannot *grant itself* host access by rewriting its
  config. **`up` is held to the approval in argv form too**, since a Feature or image tag can move
  between check and `up` (measured): every devcontainer invocation runs docker through the **docker
  guard** (`--docker-path`), which refuses any `docker run`/`create`/`build` asking for more than
  Drydock's own flags plus the approval, any option or command it does not know, and anything that
  creates a container when it has no policy — before docker runs (§6, "The docker guard").
- **No Docker socket in any workspace container.** Docker-out-of-Docker would let one container mount
  another's broker socket.
- **The frontend invents no state.** Every mutation returns `202`; the only local state a click may
  create is "a request is in flight". Entity state is written by exactly one thing — the reducer over
  the SSE stream — and the `202` body is discarded. Patching an entity from a mutation response looks
  like it works and diverges the moment a second device acts. Nothing is ever framed: the UI sends
  `frame-ancestors 'none'` and `frame-src 'none'`, so a preview can neither embed the control plane
  nor be embedded in it. See frontend §2.1 and §8.
- **Redact by default.** Passwords, login codes, session cookies, GitHub tokens, secret values, and
  PTY buffers never reach the event log, a file, or Caddy's access log. The login prompt does not echo
  (Spike 01), but Drydock holds the code in memory where a request log or crash dump can leak it, and
  non-echoing is undocumented behaviour of a pinned version: redact the code, and never store the PTY
  buffer verbatim.
- **`drydock-secrets export` must be silent on success and `exit` non-zero on failure.** It runs as a
  prelude before *every* Bash command (Spike 03), so anything it prints is prepended to every tool
  result the agent reads, and an `exit` is what makes a broker outage fail the command loudly instead
  of running tests with the secret silently missing. Diagnostics go to Drydock's event log over the
  socket. **Its exit status alone does nothing**: `eval "$(…)"` evaluates the empty string a failed
  substitution leaves and succeeds, so the helper also prints `exit 69` for the `eval` to run. A test
  of this asserts the command *after* the prelude did not run — a test of the helper's status passes
  while the bug stands.
- **The `CLAUDE_ENV_FILE` script is one constant line:**
  `eval "$(drydock-secrets export || echo exit 69)"`. Its *text* is cached per session and passed to
  every command shell as `argv` (Spike 03), so a text change needs a supervisor restart and resolved
  values must never be inlined — invoking the helper is the only reason values stay out of `ps`. The
  `|| echo exit 69` is not decoration: a helper that cannot run at all prints nothing, and without it
  `eval ""` succeeds and the command runs without its secrets.
- **Stop a `remote-control` server with `SIGTERM`, escalating to `SIGKILL` only on timeout.** A clean
  stop deregisters the folder; a `SIGKILL` of a server with no live session blocks the next start for
  one to three minutes (Spike 02) — a wait, not a crash, which must not consume the restart budget.
  **Match it on `already served by a terminal`, never on `409`** (`2.1.246` prefixed the status code
  and `2.1.289` dropped it), so a classifier keyed on the number silently reclassifies the one
  retryable refusal.
- **Two of the three config gates *hang* rather than fail.** A missing `remoteDialogSeen` waits on
  `Enable Remote Control? (y/n)`, and a missing trust record waits on `Trust <dir>? [y/N]` — the latter
  only on a PTY, which is what the supervisor gives it; redirected, it exits `1`. A hang has no error
  string, so those scenarios assert a **timeout**.
- **Pin the Claude Code version and set `DISABLE_AUTOUPDATER=1`** in the Feature. An unpinned rebuild
  once silently moved `2.1.246` → `2.1.289` and falsified three recorded findings. Bump only through
  the §11.1 ritual: two places scrape Claude Code's terminal output, and every spike measures
  undocumented internals of one version. **Current state: all four Claude Code spikes are re-measured
  on `2.1.289`.** **Re-run all four Claude Code harnesses (`00`–`03`) on every bump** and update the
  version here, in the Feature, in `internal/classify`, in `internal/claudetest` (with its pins), and
  in each spike report. Spike `04` is about browser behaviour and has its own trigger (testing §11.6).
- **The shared credential volume must be a local Docker volume — never NFS or CIFS.** Claude Code's
  cross-container refresh lock is a `mkdir(2)` lockfile at `$CLAUDE_CONFIG_DIR/.oauth_refresh.lock`,
  and network filesystems do not make `mkdir` atomic (Spike 00). Enforced twice: step 4 refuses a
  non-local driver or driver options, and `drydock-preflight` refuses a network filesystem at
  `CLAUDE_CONFIG_DIR`. Nothing in Drydock removes the volume.
- **Anything Drydock writes into the shared `.claude.json` takes Claude Code's own lock**
  (`$CLAUDE_CONFIG_DIR/.claude.json.lock`, a `mkdir` lock, re-reading under it), then writes temp +
  `rename(2)`, merging so `oauthAccount` survives. It waits and never steals the lock.
- **Never reap `.oauth_refresh.lock`.** An abandoned lock self-heals after 60s, and a cleanup pass
  racing the 5s heartbeat is strictly worse than waiting (Spike 00).
- **A blanked credential is not an expired one.** On a dead login Claude Code rewrites
  `.credentials.json` with `accessToken: ""` / `refreshToken: ""` / `expiresAt: 0`, killing every
  container on the volume at once. The expiry watch reports it as "signed out, sign in again", never as
  part of the expiry countdown (§7.3). That countdown is the **login's** — `refreshTokenExpiresAt` —
  never the access token's `expiresAt`, which a real sign-in sets about eight hours out: counting down
  on that greets every login with a false warning and teaches the operator to ignore the one banner
  that announces `blanked`.

## Working conventions from the design

- **Secrets are default-deny.** No `secret_grant` row, no secret. The required `reach` field ("what
  can someone do with this?") is a real control, not documentation.
- **Sessions are observed, not owned.** Drydock never creates a Remote Control session; it tails
  `--verbose` output and upserts `rc_session` rows. If the cache drifts, the Claude app is right. The UI
  links out with a count; it does not reimplement a session browser. The handle is the **environment
  id** (`env_…`, one per workspace, survives restart), linked as `claude.ai/code?environment=<id>`;
  `Capacity: N/4` gives the count. Scrape **ids**, not URLs, **only from OSC 8 hyperlink targets** — the
  URL and its label run together in the byte stream (Spike 02), and a bare `session_…` match would accept an id the model printed in its own prose. The terminator is BEL
  on `2.1.289`, ST on `2.1.246`.
- **Agent branches go under a `drydock/` prefix**, configured in the Feature. Commits use the App's
  bot identity.
- **Nothing stops a workspace automatically.** No idle reaper — mistaking "an agent thinking" for
  "idle" destroys work. Capacity is a hand-managed cap plus a stop button.
- **Every step of clone → container writes an event**, so a failure names its step.
- **A fault with one cause gets one message and one button, wherever it manifests.** A blanked
  credential takes every workspace down at once; rendering each card from its own row produces ten
  *Restart session server* buttons, none of which can work. So fleet-wide identity state overrides
  per-card presentation, narrowly: only what the credential broke is replaced, and a card whose image
  build failed keeps its own status and action. A per-card action that cannot work is never shown
  disabled; it is replaced by the action that can (frontend §6.6).
- **`blanked` and `absent` are different sentences.** *"Signed out. Sign in again."* means everyone
  just lost access; *"No one has signed in yet."* is the first-run state. `auth status --json` says
  `loggedIn:false` for both and only the credential file separates them (§7.3).
- **The context rule.** #83, #85 and #94's probe cleanup each ran under a context from the wrong
  parent. (1) A request's context is for the synchronous part of a handler only. (2) Background work
  runs under its component's context (a `life.Group`'s, or, until R1 reaches it, the component's `base`). (3) Bookkeeping or cleanup
  owed after a cancellation uses `sys.Cleanup(parent, clock, d)`: `WithoutCancel` plus a bound on the
  injected clock. (4) Shared, joinable work never runs under a caller's context. `TestContextRule`
  (`internal/sys/contextrule_test.go`) parses every non-test file and fails, by file and function, on
  `context.Background`/`TODO`/`WithoutCancel`, a wall-clock deadline (`context.WithTimeout`,
  `WithDeadline` and their `Cause` forms) or a wall-clock `time` call (`Now`, `After`, `Sleep`,
  timers and tickers) outside `cmd/`, `internal/sys` and the helper packages `claudetest` (with
  `fakeclaude`), `githubtest` and `logintest`, unless its allowlist names it with a reason. Entries
  marked *R1 debt* are wall-clock bounds R1 moves to the injected clock. A new entry is a design
  decision, so its reason must say which rule makes it right.

## Build order

Design §14 orders the phases so the riskiest unknown resolves first.

0. **Spikes** — done (`00`–`03` on Claude Code, `04` the browser tier's local CA). Still open: whether
   a `--spawn worktree` path needs its own trust record.
1. **Front door** — done, and deployed for real.
2. **Walking skeleton** — done, reboot drill included.
3. **Credentials** — done.
4. **Secrets** — done.
5. **Claude** — done on the owner's real account. Still open (§15.1): whether a real login's thirty
   days are the server's figure or Claude Code's default.
6. **Livability** — done.

Phases 2–4 are independently useful; if Phase 5 is blocked by something in §2, what remains is still
most of the value.

## Testing

`docs/design/testing/testing-design.md` is the plan. Four tiers — unit, component (fake subprocesses,
real sockets and SQLite), container (real DinD), browser (real Chromium and Caddy) — plus rituals
triggered by an event rather than a commit. A test lives in the cheapest tier whose reach includes the
boundary its assertion is about. Six things change how code gets written here:

- **Every negative test carries a positive control in the same function.** Nearly every invariant
  is a prohibition, and a prohibition is satisfied by a binary that does nothing. A test that would
  still pass with the feature deleted is not a test of the invariant.
- **The canary sweep.** Component tests keep all mutable state under one temp root, seed
  high-entropy canaries, and grep the whole tree plus the SQLite file's raw bytes afterwards — how
  "redact by default" and §4's deliberate schema absences are covered without a sink list anyone must
  extend. Two sinks are not files and are swept explicitly: `/proc/<pid>/cmdline` (the prelude's text
  is `argv` on every command) and the HTTP path that carries the login code.
- **The route table is data** — `{method, pattern, handler, mutating}` — so the auth, `Origin`, CORS
  and mux-separation meta-tests enumerate it and a route added later is covered without editing a test.
- **Time, disk, and randomness are injected**, and subprocesses resolve by `PATH` so a fake binary
  can stand in. A Go mock of the `devcontainer` CLI tests our belief about it; a fake binary tests the
  argv we actually build, which is a security surface.
- **To prove a value is in no process's argv, trace it.** A wrapper around `git` sees only git's own
  argv; `GIT_TRACE` from that wrapper also records the processes git starts, such as a credential
  helper (`internal/clone`).
- **The `drydock.workspace` label key is configuration, not a constant.** A second Drydock on the same
  daemon — which is what a test is — would otherwise adopt and destroy real workspaces.

Two Phase 0 findings a correct-looking test passes: **exit status is not a discriminator** (all four
`remote-control` startup refusals exit `1`, so the classifier's test feeds four fixtures with the same
exit code and demands four verdicts), and **a missing `remoteDialogSeen` hangs rather than failing**,
so that scenario asserts a *timeout*.

A design finding closed in prose with no test is one that reopens quietly: each has test rows in
testing §8.

## Brand

`docs/design/brand/brand-design.md` settles the icon and records why each alternative lost. The mark
is a section through a **drained** basin holding a container on keel blocks, `DD` stencilled on its
face — no water (every "harbour" variant was rejected) and nothing Docker-adjacent. Easy to get wrong:

- **Two forms, different jobs.** `drydock-mark.svg` in-page where the ground is known;
  `drydock-badge.svg` (knocked out of a steel rounded square) wherever the surface is someone else's —
  favicon, app icon. `drydock-mark-mono.svg` inherits `currentColor` and carries **no letters**.
- **The SVGs and the PNGs are not generated from one source.** Change a path in `icons/` and change
  `render-icons.mjs` too and re-run it; nothing warns you when they drift. It takes a JSON argv of
  `[path, size, mode, colors]` entries and uses only Node builtins:
  ```sh
  node docs/design/brand/render-icons.mjs '[["docs/design/brand/icons/favicon-16.png",16,"badge","steel"],
    ["docs/design/brand/icons/favicon-32.png",32,"badge","steel"],
    ["docs/design/brand/icons/apple-touch-icon-180.png",180,"bleed","steel"],
    ["docs/design/brand/icons/github-app-200.png",200,"bleed","steel-lift"],
    ["docs/design/brand/icons/github-app-dev-200.png",200,"bleed","steel-inverse"]]'
  ```
- **Re-render `icon-preview.html` after any geometry change** — it is what measures contrast (why
  the badge ground is steel and the badge a single knockout).
- **The badge ground is deliberately not a brand colour**, so `#1d4ed8` keeps meaning *API traffic*
  and `#6d28d9` *preview origin* in the diagrams.
- **The GitHub App logos are opaque, full-bleed PNGs, and the two Apps differ by inversion.** Prod
  (`github-app-200.png`) uses the lighter steel `#64748b` (`#475569` scores 2.50 on GitHub's dark
  page); dev is the same image inverted, not a new hue, since every hue is semantic in the diagrams.
  They are uploaded to each App's settings by hand.

Still open: a wordmark (the stencilled `DD` is gone by 20 px).

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

Design docs carry no version number, date or changelog in their header; a change is described in the PR (its squash commit is the history). Do not add one back: every parallel PR conflicted on that line. (The security review keeps its date, because it is a dated record of the designs as they stood.)

Package detail lives beside the code — the Go package comment, or a `README.md` in `web/`, `feature/`,
`deploy/` and each `test/` directory — not here. Keep this file to what an agent must know before
touching anything.
