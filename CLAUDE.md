# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

**Design-only. There is no application source code yet.** The repository contains the overall design
document (`docs/design/overall/drydock-design.md`, draft v7), supplemental ones on port forwarding
(`docs/design/port-forwarding/`, draft v4), testing (`docs/design/testing/`, draft v4) and the Vue
frontend (`docs/design/frontend/`, draft v4), an adversarial security review
(`docs/design/security-review.md`), their SVG diagrams, a devcontainer definition, and the **four
completed Phase 0 spikes** with their harnesses under `docs/design/spikes/`. There are no build,
lint, or test commands because nothing is built yet — the testing document specifies what they will
be.

The devcontainer (`.devcontainer/devcontainer.json`) carries the full toolchain: Go (with
golangci-lint), Node, **docker-in-docker**, the `devcontainer` CLI, Caddy, `gh`, and
`sqlite3`/`socat`/`nc`/`jq`.

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
  `--json` result.
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
  secret silently missing. Diagnostics go to Drydock's event log over the socket.
- **The `CLAUDE_ENV_FILE` script is one constant line that delegates to the helper.** Its *text* is
  cached per session and is passed to every command shell as `argv` (Spike 03). So a text change
  needs a supervisor restart, and resolved values must never be inlined — the helper is *invoked*
  from the prelude, which is the only reason values stay out of `ps`.
- **Stop a `remote-control` server with `SIGTERM`, escalating to `SIGKILL` only on timeout.** A clean
  stop deregisters the folder and lets the next start in immediately; a `SIGKILL` of a server with
  no live session blocks the next start with a `409` for one to three minutes (Spike 02). That `409`
  is a wait, not a crash, and must not consume the restart budget.
- **Pin the Claude Code version and set `DISABLE_AUTOUPDATER=1`** in the devcontainer feature. Two
  places scrape Claude Code's terminal output (the login URL, the session URLs); a background update
  would change them without warning. Every one of the four spikes added a harder reason: all of them
  measure undocumented internals of **Claude Code `2.1.246`** — the refresh lock, the login flow and
  `auth status` schema, the reconnect behaviour and the three config gates, and the per-command
  prelude. **Re-run all four harnesses in `docs/design/spikes/` on every bump** and update that
  version here.
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
  gives the count for free. Scrape **ids**, not URLs — per-session URLs come wrapped in OSC 8
  hyperlink escapes, so the URL and its label run together in the byte stream (Spike 02).
- **Agent branches go under a `drydock/` prefix**, configured in the feature rather than left to the
  model to remember. Commits use the App's bot identity.
- **Nothing stops a workspace automatically.** No idle reaper — distinguishing "idle" from "an agent
  thinking" wrong destroys work. Capacity is a hand-managed cap plus a stop button.
- **Every step of clone → container writes an event**, so a failure names its step rather than
  reporting "failed".

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
1. **Front door** — socket listener, `drydock passwd`, session middleware, `Origin`/`Host` checks,
   Caddy block. *Nothing else gets built until every route without a cookie returns 401.*
2. **Walking skeleton** — repo list, clone, `devcontainer up`, states, SSE, boot reconciliation.
3. **Credentials** — token broker, per-workspace socket, git credential helper, `gh` shim.
4. **Secrets** — encrypted store, `GET-SECRETS`, `drydock-secrets export`, grants UI.
5. **Claude** — shared credential volume, login handshake, supervisor, session discovery, expiry.
6. **Livability** — stop/rebuild/delete, concurrency cap, disk and session counts, log viewer.

Phases 2–4 are independently useful; if Phase 5 is blocked by something in §2, what remains is still
most of the value.

## Testing

`docs/design/testing/testing-design.md` is the plan. Four tiers — unit, component (fake subprocesses,
real sockets and SQLite), container (real DinD), browser (real Chromium and Caddy) — plus rituals
triggered by an event rather than a commit. A test lives in the cheapest tier whose reach includes the
boundary its assertion is about. Five things from it change how code gets written here:

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

## Docs

Design docs live under `docs/design/<scope>/`, with diagrams in a sibling `diagrams/` directory.
Spike results live under `docs/design/spikes/`, each one a numbered report next to the re-runnable
harness that produced it — a spike whose evidence cannot be re-checked against a new Claude Code
version is worth very little. Spike 00's harness is `harness/`; later ones are `harness-NN-<topic>/`,
each with a README naming the command per result. Keep that pairing: the report cites the mode that
produced each number, so a finding can be re-measured rather than re-argued.

Diagrams ship as light/dark SVG pairs (`NN-name-light.svg` / `NN-name-dark.svg`) referenced from a
`<picture>` element with a `prefers-color-scheme: dark` source, and every one carries a descriptive
`alt` and a `**Fig N** —` caption explaining what the reader should take from it. Match that pattern
when adding diagrams.
