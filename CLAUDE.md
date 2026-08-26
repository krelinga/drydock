# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

**Design-only. There is no source code yet.** The repository contains one design document
(`docs/design/overall/drydock-design.md`, draft v3) plus its SVG diagrams, and a devcontainer
definition. There are no build, lint, or test commands because nothing is built yet.

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
- **These variables must stay unset in every container:** `ANTHROPIC_BASE_URL` (or point at
  `api.anthropic.com`), `DISABLE_TELEMETRY`, `DO_NOT_TRACK`,
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `DISABLE_GROWTHBOOK`. Any of them disables Remote
  Control while everything still builds and starts. They are also on the reserved-secret-name list.
- **Auth is middleware around the whole mux**, so a route added later is protected by default. Only
  the sign-in POST is unauthenticated, and it is rate-limited and lockout-guarded.
- **No route returns a secret value** — no reveal button, no edit form, no re-auth escape hatch. The
  schema has no `value` column, and it should stay that way.
- **Store no credential but the App private key.** GitHub tokens live in a bounded in-memory cache;
  `token_grant` records that a token was issued, never the token. `auth_session.id` is the SHA-256
  of the cookie value, so a stolen DB file yields no usable cookie. Secret values are XChaCha20-
  Poly1305 ciphertext with the secret id as AAD.
- **The App key and the secrets master key never enter the environment** — mode `0400` files read
  once at startup. Environment variables leak into `/proc`, crash reports, and every child process.
- **No Docker socket in any workspace container.** Docker-out-of-Docker would let one container
  mount another's broker socket.
- **Redact by default.** Passwords, login codes, session cookies, GitHub tokens, secret values, and
  PTY buffers never reach the event log, a file, or Caddy's access log. The login PTY buffer
  contains the one-time code — scrape, match, redact, then store.
- **Pin the Claude Code version and set `DISABLE_AUTOUPDATER=1`** in the devcontainer feature. Two
  places scrape Claude Code's terminal output (the login URL, the `claude.ai/code/<id>` session
  URLs); a background update would change them without warning.

## Working conventions from the design

- **Secrets are default-deny.** No `secret_grant` row, no secret. The required `reach` field ("what
  can someone do with this?") is a real control, not documentation.
- **Sessions are observed, not owned.** Drydock never creates a Remote Control session; it tails
  `--verbose` output continuously and upserts `rc_session` rows as sessions appear. If the cache
  drifts, the Claude app is right and Drydock is wrong. The UI links out with a count; it does not
  reimplement a session browser.
- **Agent branches go under a `drydock/` prefix**, configured in the feature rather than left to the
  model to remember. Commits use the App's bot identity.
- **Nothing stops a workspace automatically.** No idle reaper — distinguishing "idle" from "an agent
  thinking" wrong destroys work. Capacity is a hand-managed cap plus a stop button.
- **Every step of clone → container writes an event**, so a failure names its step rather than
  reporting "failed".

## Build order

§14 of the design doc orders the phases so the riskiest unknown resolves first. Follow it:

0. **Spikes** — shared credential volume under concurrent refresh (the one open question that can
   force a redesign, §15.1), a scripted PTY login handshake, supervisor restart survival, and
   whether `CLAUDE_ENV_FILE` is re-read per Bash command.
1. **Front door** — socket listener, `drydock passwd`, session middleware, `Origin`/`Host` checks,
   Caddy block. *Nothing else gets built until every route without a cookie returns 401.*
2. **Walking skeleton** — repo list, clone, `devcontainer up`, states, SSE, boot reconciliation.
3. **Credentials** — token broker, per-workspace socket, git credential helper, `gh` shim.
4. **Secrets** — encrypted store, `GET-SECRETS`, `drydock-secrets export`, grants UI.
5. **Claude** — shared credential volume, login handshake, supervisor, session discovery, expiry.
6. **Livability** — stop/rebuild/delete, concurrency cap, disk and session counts, log viewer.

Phases 2–4 are independently useful; if Phase 5 is blocked by something in §2, what remains is still
most of the value.

## Docs

Design docs live under `docs/design/<scope>/`, with diagrams in a sibling `diagrams/` directory.
Diagrams ship as light/dark SVG pairs (`NN-name-light.svg` / `NN-name-dark.svg`) referenced from a
`<picture>` element with a `prefers-color-scheme: dark` source, and every one carries a descriptive
`alt` and a `**Fig N** —` caption explaining what the reader should take from it. Match that pattern
when adding diagrams.
