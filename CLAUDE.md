# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

**Mostly design, with the first code in.** The repository contains the overall design
document (`docs/design/overall/drydock-design.md`, draft v8), supplemental ones on port forwarding
(`docs/design/port-forwarding/`, draft v4), testing (`docs/design/testing/`, draft v6) and the Vue
frontend (`docs/design/frontend/`, draft v4), a settled brand mark (`docs/design/brand/`, v1.0,
with the shipping icon assets), an adversarial security review
(`docs/design/security-review.md`), their SVG diagrams, a devcontainer definition, and **five
completed spikes** with their harnesses under `docs/design/spikes/` — the four Phase 0 ones plus
`04`, the browser-tier local CA, which the testing plan asked for later.

The Go module is `github.com/krelinga/drydock`. What exists so far is the **five testability seams**
from testing §5 and nothing else — interfaces, the declared HTTP surface, and the fixture layout:

```sh
go build ./... && go vet ./... && go test ./...   # the whole suite today
gofmt -l .                                        # must print nothing
```

| Package | Is |
|---|---|
| `internal/api` | The route table as **data**, both muxes, the gate interface, the error envelope — and ten meta-tests that walk the table. The only package with real logic yet. |
| `internal/sys` | `Clock`, `DiskUsage`, `Random`. Never call `time.Now()` directly. |
| `internal/subproc` | An invocation described as data, resolved by `PATH` or an injected `Resolver`. No shell anywhere, deliberately. |
| `internal/config` | Settings that must not be constants, `LabelPrefix` chief among them, plus a `Validate` that refuses configurations which silently undo a design property. |
| `internal/classify` | The five classifier **signatures**. They `panic` rather than return a plausible zero — see below. |
| `test/fixtures/` | The corpus layout and its recording rules. **Empty**; recording it is what the testing plan still owes Phase 0. |

Three things about that code worth knowing before extending it:

- **Handlers are `nil` for routes a later phase owns**, and `Build` mounts a `501`. That is deliberate:
  the table is the complete contract from the first commit, reviewable against §5 as a list, and the
  gate is provably applied to routes nobody has written yet. **The auth gate runs before that `501`**
  — otherwise an unauthenticated caller could tell a declared route from a nonexistent one and read
  the API surface off a server it cannot use. There is a test for the ordering; do not reorder it.
- **The classifiers panic on purpose.** A classifier that quietly returns `IdentityOK` for bytes it
  cannot parse is exactly the failure the fixture corpus exists to prevent, so an unimplemented one
  fails loudly rather than plausibly.
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
  prelude. **Re-run all four Claude Code harnesses (`00`-`03`) on every bump** and update that version
  here. Spike `04` is about browser behaviour, not Claude Code, and has its own trigger.
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

## Brand

`docs/design/brand/brand-design.md` settles the icon at v1.0 and records why each alternative lost.
The mark is a section through a **drained** basin holding a container clear of the floor on keel
blocks, `DD` stencilled on its face — the drained basin is the only idea it carries, which is why
drawing water in it (every "harbour" variant) was rejected, along with anything Docker-adjacent,
since the design treats Docker as an implementation detail it never scrapes. Phase 1 needs a favicon
and a header mark, so these ship with the front door.

Four things about the assets that are easy to get wrong:

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
    ["docs/design/brand/icons/apple-touch-icon-180.png",180,"bleed","steel"]]'
  ```
  As of this writing the committed PNGs match the renderer byte-for-byte.
- **Re-render `icon-preview.html` after any geometry change.** It is what caught every measured
  problem in the note — the badge ground is steel because ink navy scores **1.05** against a dark
  page, and the badge is a single knockout because keeping the accent container scores **1.00**
  between the letters and their ground.
- **The badge ground is deliberately not a brand colour.** Steel leaves `#1d4ed8` meaning *API
  traffic* and `#6d28d9` meaning *preview origin* in the diagrams, rather than making a brand colour
  and a semantic colour the same colour.

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
