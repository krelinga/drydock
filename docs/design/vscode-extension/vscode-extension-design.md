# Drydock for VS Code

*A VS Code extension that drives Drydock from the command palette: list the repositories, clone one, follow it to running and open it in a new window, all without leaving the editor or touching the mouse. It is a second client of the same API, so it inherits the web UI's rules rather than inventing its own: every mutation is a `202` and a wait, and the event stream is the only thing that changes what it shows.*

**Status** living design document. Its history is the git log (`git log -p -- docs/design/vscode-extension/vscode-extension-design.md`). Nothing here is built yet.

**Supplements** [`../overall/drydock-design.md`](../overall/drydock-design.md) (§5 the API, §6 *Opening a workspace in VS Code*, §13 security) · [`../frontend/frontend-design.md`](../frontend/frontend-design.md) (§2.1, §4, §6.5a, §6.6) · [`../testing/testing-design.md`](../testing/testing-design.md)

**Runtime** the operator's own VS Code, on the laptop (`extensionKind: ["ui"]`), talking HTTPS to Caddy at the UI origin

**Reach** one operator, one Drydock, on the LAN. The same reach as the web UI

**Out of scope** a session browser · a terminal or log viewer beyond an output channel · VS Code forks that cannot run Microsoft's Remote extensions · managing more than one Drydock at once (see §14)

## Decisions

| | Decision | Where |
|---|---|---|
| Where it runs | The laptop's extension host (`extensionKind: ["ui"]`). It works before any remote window exists and can open new windows. | §3.1 |
| How it reaches Drydock | HTTPS to Caddy at the UI origin, like a browser. Never the Unix socket. | §3.2 |
| Auth, v1 | **(a) Password sign-in, with the session cookie in `SecretStorage`.** No server change. The extension never stores the password and never retries a sign-in on its own. | §4 |
| Auth, later | **(b) Scoped, revocable API tokens**, introduced together with the first phase where the extension should hold *less* power than a browser session. Once a token's scope leaves out approvals and secrets, "the extension cannot approve" is enforced by the server, not just by the client choosing not to. | §4.5 |
| `Origin` | The extension sends the literal UI origin on every mutation. This forges nothing: the check defends browsers against third-party pages, and a client that already holds the credential was never inside its threat model. The server does not change. | §4.3 |
| State | **The web UI's reducer, unchanged, shared as source** from a DOM-free `web/src/core/`. The extension follows "the frontend invents no state" with the same code. | §5 |
| Commands | Every action is a `drydock.*` command in the palette. A single *Go to Workspace…* quick pick is the hub. One default keybinding, and a test checks it for conflicts. A tree view is the secondary surface. | §6 |
| Connect | `vscode.openFolder` on the `vscode-remote://` form of the server's own link, in a new window. Not `openExternal` on the `vscode://` link. Remote-SSH and Dev Containers are checked for at connect time, **not** declared as `extensionDependencies`. | §7 |
| Commands as attack surface | Any extension host can call our commands, including the one inside a workspace container. So commands take no arguments that can cause a mutation, return nothing, and confirm every mutation in UI that only the operator can answer. In a container window the extension does not mutate at all. | §8 |
| Version skew | A new authenticated `GET /api/meta` returns `{version, api}`. The extension declares the `api` range it speaks. A Go test pins the routes and event kinds the extension uses. Outside the range, a mutation is replaced by *Install the matching extension*. | §9.1 |
| Release | A `.vsix` uploaded to every Drydock release under a fixed name (`drydock-vscode.vsix`) by the existing release-please flow. Not the Marketplace, at least at first. | §9.2 |
| Phasing | **E1**: sign-in, catalog, clone, follow, connect. **E2**: start, stop, rebuild, delete, logs, Claude status. **E3**: the Claude login, ports. **Approvals and secrets stay in the web UI** unless §10's conditions are met. | §10 |
| Testing | Unit (Vitest, no `vscode`) · manifest contract · **client tier** (the protocol client against real Drydock behind real Caddy, no VS Code) · **extension-host tier** (`@vscode/test-cli`, headless under xvfb, the packaged `.vsix`) · a nightly keyboard smoke test · and rituals for the real Remote-SSH attach, which CI cannot reach. | §12 |

## 1. Problem & goals

The operator spends the working day in VS Code on a laptop, connected over Remote-SSH to the server that runs Drydock. Today, getting from "I want to work on repo X" to an editor attached to X's container means switching to a browser tab: find the repo, press *Clone*, watch the card, and press *Open in VS Code* (design §6, *Opening a workspace in VS Code*), which bounces through the OS URL handler and a confirmation dialog back into VS Code. The extension removes the browser from that loop.

### Functional requirements

- **E1, the first iteration:** see the repositories the GitHub App can reach (`GET /api/repos`), clone one (`POST /api/workspaces`), follow its eight steps to `running`, and connect: open the running container in a new VS Code window at the clone's folder. Also: the running workspaces, *Connect* to any of them, and *Open in Drydock* (the web UI page) for anything the extension does not do itself.
- **From the keyboard alone.** Every action is a command. The hub is one quick pick reachable by one shortcut, and the whole of E1's flow (shortcut, type part of a name, Enter, Enter) needs no mouse.
- **Eventually, most of the web UI.** E2 and E3 add the workspace lifecycle, logs, the Claude identity and login, and ports (§10). Host-access approvals and secrets are argued separately and may never come (§10.4).
- **Never a second opinion about state.** What the extension shows is what the server's event stream says, through the same reducer the web UI runs (§5). A clone pressed on the phone shows up in the editor, and the reverse.

### Non-goals

- **Not a replacement for the web UI.** The phone and tablet stay first-class clients (overall §13.1). The extension is a second front end, and the web UI stays the complete one.
- **Not a Drydock inside VS Code.** No webview hosting the web UI: the UI sends `frame-ancestors 'none'` (frontend §8), and a webview would bring back a browser with an ambient credential, which §8 below works to avoid.
- **Not a remote-development tool.** Remote-SSH and Dev Containers do the connecting. The extension computes nothing about containers. It hands VS Code the authority Drydock already builds (`internal/vscode`).
- **No Marketplace listing in E1.** §9.2.

## 2. Binding constraints

Six facts shape the design. Three of them overturn the obvious approach.

### 2.1  The laptop must reach Caddy, not just the server's SSH port

A UI-side extension makes its HTTP requests from the laptop. It reaches Drydock exactly as the laptop's browser does: by resolving the UI host and connecting to Caddy on `:443` over the LAN. It does **not** ride the Remote-SSH connection. If the laptop can SSH to the server but not reach Caddy (off the LAN, over a jump host), the extension cannot reach Drydock, and neither can the browser. That is the web UI's reach, inherited on purpose (overall §13.1: LAN, behind Caddy). A relay through the SSH connection is possible and is listed in §14 with what it would cost.

### 2.2  The extension host is Node, and Node is not a browser

Four browser behaviours the web UI relies on are absent in the extension host, and each needs a deliberate replacement:

| Browser gives the web UI | Extension host has | Consequence |
|---|---|---|
| A cookie jar that stores `__Host-drydock` and sends it back | `fetch` with no jar | The extension reads `Set-Cookie` itself, keeps the value in `SecretStorage`, and sends `Cookie:` itself (§4.1). |
| An `Origin` header on every mutation, set by the browser and unforgeable by page script | No `Origin` unless the code sets one | The extension sets it (§4.3). |
| `EventSource`, with `Last-Event-ID` reconnects | No `EventSource` by default, and the standard one cannot send a `Cookie` header anyway | A small fetch-based SSE reader (§5.3). The server already accepts the resume point as the `Last-Event-ID` header (`internal/api/events_routes.go`). |
| A user-agent string that `deviceLabel` turns into "Mac Chrome" | `node` | The device list would say *Unknown device*. A one-line server change (§11). |

### 2.3  Any extension host can run our commands, including one inside a workspace container

VS Code routes `vscode.commands.executeCommand` across extension hosts. It is how a UI extension and a workspace extension are meant to talk to each other. So a command `drydock.*` registered on the laptop can be invoked by an extension running in the remote extension host of an **attached workspace container**, which is code the repository (and the agent working in it) can put there: it can drop an extension into the container's own `~/.vscode-server/extensions`, and the attach flow installs extensions that the container's configuration metadata names. The web UI never faced this. A page cannot call into another tab. §8 is the design's answer, and it is the main thing the owner's earlier positions did not account for.

### 2.4  *Connect* depends on two proprietary extensions

The link opens `attached-container+<hex>@ssh-remote+<host>`. Resolving it takes Microsoft's **Remote - SSH** (`ms-vscode-remote.remote-ssh`) for the parent authority and **Dev Containers** (`ms-vscode-remote.remote-containers`) for the attached container. Both are closed source, licensed only for Microsoft's VS Code builds, and unavailable on Open VSX. Everything else in the extension (catalog, clone, lifecycle) works without them. §7.3 explains why that argues against declaring them as dependencies.

### 2.5  The UI no longer ships with the server

Today `internal/web/dist` is embedded in the binary, so the UI and the API always match. A `.vsix` the operator installed last month can be older or newer than the server it talks to. Nothing in the API says which version it is. The only version anywhere is `drydock version` on the host (`cmd/drydock/main.go`, stamped by `-ldflags -X main.version=`). §9 adds the handshake.

### 2.6  The link is in views, not in events

A workspace's `vscode` field (`{configured, url}`) is filled from Docker as `GET /api/workspaces` and `GET /api/workspaces/{id}` are served (`internal/vscode` `Linker.Fill`), never stored, and **carried by no event**. A rebuild renames the container. So, like the web UI (`web/src/stores/reducer.ts` `becameRunning`, `vscodeLink`), the extension refetches the view when a `workspace.state` moves to `running`, and trusts only a link read from a view no older than the latest state change. Following progress on the stream is not enough to connect.

## 3. Shape

### 3.1  Where it runs: the laptop (`extensionKind: ["ui"]`)

A UI extension runs in the local extension host whatever window it is in: local, SSH, or attached container. That is what E1 needs:

- **It works before connecting anywhere.** *Clone, then connect* starts from a window that may have no remote at all.
- **It can open a new window** on a remote authority (§7). A workspace-side extension could too, but only once a remote existed to run it in.
- **One installation.** A workspace-kind extension would have to be installed on the server, and every attached container would bring its own copy.

A workspace-side extension (on the server, inside the SSH remote) was the alternative. Its one real advantage is §2.1's: it could reach Caddy from the server itself. It loses on everything else. It is absent until a remote window exists, and it would run as the SSH user, who has Docker access and is therefore root on the host (overall §13.4). It would then be tempted to read the Unix socket directly, which §3.2 rules out.

### 3.2  How it reaches Drydock: Caddy, at the UI origin

The extension calls `https://<ui-host>/api/...` exactly as the web UI does: same Caddy site block, same `Host`, same certificate, same gate (`internal/api/mux.go` `wrap`: Host, then session, then `Origin`, then the handler). It never touches `/run/drydock/http.sock`:

- **The socket is the trust boundary.** It is `0660`, group `drydock`, and Caddy is the only member (CLAUDE.md, *No TCP listener*). Reaching it from the laptop would mean either adding the SSH user to `drydock`, which breaks that rule, or forwarding the socket over SSH, which makes the SSH user's account a second front door.
- **Caddy does work the server relies on.** It applies strict `Host` matching, and it *replaces* `X-Forwarded-For`, which is the address the sign-in lockout keys on (`internal/api/session_routes.go` `clientIP`). A request arriving straight at the socket has no forwarded address, so every such client would share one lockout bucket under the literal key `unknown`.
- **One path, one set of tests.** Everything the browser tier has proved about this path (testing §10) applies to the extension unchanged.

### 3.3  Alternatives considered

| Alternative | Why not |
|---|---|
| A webview showing the web UI | `frame-ancestors 'none'` refuses it, deliberately. A webview with its own cookie would bring back a browser with an ambient credential inside VS Code, which is the CSRF model §4.3 relies on *not* existing here. It also does nothing for the command palette. |
| A `drydock` CLI client that the extension shells out to | It adds a second binary on the laptop to version and ship, and a credential on disk outside `SecretStorage`. The extension would still need its own state handling for progress. The CLI is worth having on its own merits (§14), but not as the extension's engine. |
| The Unix socket, from a workspace-side extension | §3.1 and §3.2. |

## 4. Authentication

There are two options. Both leave the gate's order, the `Origin` rule and the single unauthenticated route as they are.

### 4.1  Option (a): the existing sign-in, cookie in `SecretStorage`

*Drydock: Sign In* asks for the password in `showInputBox({ password: true, ignoreFocusOut: true })` and posts it to `POST /api/auth/session` (`auth.session.create`) with the UI origin as `Origin`. It reads `__Host-drydock` out of the `Set-Cookie` header (exact name, and nothing else from the header) and stores **only the cookie value** in `context.secrets`. The password is dropped as soon as the request has been sent. There is no *remember password* setting.

- **Server change: none.** The handlers already serve a non-browser client: `204` with the cookie, or `200` with a `SignInNotice` (`failed_attempts`, `failed_sources`). The extension must show that notice once, as the web UI does (design §12, *Repeated failed sign-ins*).
- **Lockout and rate limit: unchanged and shared.** The sign-in goes through the same `auth.Limiter`, keyed on the laptop's address as Caddy forwards it. The extension never retries a sign-in by itself. A `429 locked_out` shows `Retry-After` and stops. A stale cookie is a `401` on an ordinary route, which counts as no sign-in attempt and no failure, so an expired session can never lock the operator out.
- **Expiry and revocation: the browser's.** 30 days absolute, 14 idle, sliding (`internal/auth/session.go`). Any `401` from any route clears the stored cookie and offers *Sign In* (frontend §4.4's one 401 path). The session appears in Settings' device list, and *Sign out everywhere* ends it. The SSE stream re-checks its session on every heartbeat (`EventRoutes.Alive`), so a revoked extension loses its stream within 20 s.
- **What it holds: a full operator session.** That is the browser row of overall §13.4 ("everything Drydock can do"), now on a second device. `SecretStorage` uses the OS keychain on macOS and Windows. On Linux it uses libsecret or kwallet when present, and otherwise falls back to a weak *basic* store. The laptop is the operator's own, and the session is no more powerful than the browser beside it. The real cost is that a client which chooses not to approve anything still *could*. §8 and §4.5 deal with that.

### 4.2  Option (b): a revocable API token minted in the web UI

Settings gains *VS Code tokens*: *Create* shows a random 32-byte token once, with a label. The table stores only `sha256(token)` (as `auth_session` does), plus the label, timestamps and a **scope**. The extension sends it as `Authorization: Bearer <token>`. `SessionGate.Authenticate` accepts the cookie *or* a bearer token and attaches the same session shape, with the scope.

- **Cleaner revocation and audit.** One token per laptop, a label that says "VS Code on krelinga-mbp", and no password typed into the editor.
- **Least privilege becomes possible.** A scope (`read`, `lifecycle`, …) that leaves out `workspaces.approve`, `workspaces.decline`, `secrets.*` and `auth.session.delete?all=true` is enforced in the gate, per route, by the table. The route table would gain a `Scope` column, and a meta-test would walk it as the existing ones do.
- **It is a new credential kind.** That means a design §4 schema row, a §13.2 row, a route pair to mint and revoke tokens (authenticated by cookie, `Mutating`, from the web UI only), the canary sweep extended to the token, and an addition to CLAUDE.md's invariant *Store no credential but the App private key* (it already allows `auth_session`'s hash, and a token hash is the same kind of thing).
- **Lockout.** Token guesses never touch the sign-in route, so the password lockout does not apply. It does not need to: 256 random bits make guessing infeasible, and a bearer lookup costs the same as the cookie lookup every request already makes, which is not rate-limited either.

### 4.3  The `Origin` check, and why a non-browser client may send the UI origin

The extension sends `Origin: https://drydock.example.com`, byte for byte the configured UI origin, on every mutating request. Under option (b) it does the same, so the gate needs no bearer exemption.

**This is acceptable because of what the check defends, and against whom.** Overall §13.3 states the threat: *a page you visit makes your own browser issue the requests*. The browser attaches the cookie because it is ambient, and the `Origin` header is the one thing the attacking page cannot choose, because browsers set it and forbid script from overriding it. The check is therefore a statement about *which page induced a credentialed browser request*. It was never client authentication: `curl` with the cookie has always been able to send any `Origin` it likes, and it passes. A client that already holds the session gains nothing by sending the origin that it could not already do. The extension is that client.

**The check's guarantee stays intact only if the extension has no ambient credential.** That is the rule that keeps the reasoning honest. The cookie lives in `SecretStorage` and is attached by our code to requests our code decides to make. Nothing in VS Code attaches it automatically: no webview cookie jar, no `http.proxy` agent, and no URL a third party can make the extension fetch. A design that let content (a webview, a link in a markdown preview, a command argument from another extension) cause a credentialed request would rebuild the browser's confused deputy inside the editor, and then sending the right `Origin` *would* be forgery. §8's rules exist to prevent that, and §12.8 tests them.

**What does not change:** the check stays exact-match and fails closed, and `OriginAllowed` is untouched. The meta-tests (`internal/api/route_test.go`) still prove it for every mutating route. The extension adds nothing to the set of `Origin` values the server accepts.

### 4.4  What each option does to the auth invariants

| Invariant (CLAUDE.md, overall §13.5) | (a) cookie | (b) token |
|---|---|---|
| *Auth is middleware around the whole mux* | Unchanged. | Unchanged in shape: `Authenticate` gains a second credential source, and both are checked before any handler. The route-table meta-tests apply as they are, plus one for scope. |
| *Only the sign-in POST is unauthenticated* | Unchanged. | Unchanged. Minting a token is authenticated (cookie) and mutating. There is no new `AuthNone` route, and the meta-test's count stays 1. |
| Sign-in lockout and rate limit | Same route, same limiter, same key. | Not involved. Tokens are unguessable (§4.2). |
| *Exact-match `Origin`, fail closed* | Unchanged. The extension sends the UI origin (§4.3). | Unchanged. No bearer exemption: the extension sends it anyway, which keeps the gate free of branches. |
| *Store no credential but the App key* | Unchanged server side. The client keeps the cookie in `SecretStorage`. | A hash per token, as for sessions. The invariant's wording is extended. |
| Redact by default | The cookie and password are canaries in the sweep (§12.8). | The token is added as a canary. |

### 4.5  Recommendation

**E1 and E2: option (a).** It costs no server change, no schema change and no design change to the auth invariants, and it holds exactly what the operator's browser holds. E1's whole surface (catalog, clone, connect) and E2's lifecycle are things the browser session should be able to do anyway.

**Before any phase where the extension should be *less* powerful than a browser: option (b), scoped.** E3's Claude login is the boundary, and approvals or secrets, if they ever come (§10.4), certainly are. As long as the extension holds a full session, "the extension does not approve" is a promise its own code keeps, and §2.3 shows other code can reach that code. A scope that leaves the approve route out makes it a property of the server, tested by a meta-test. That is the only form of the promise worth relying on.

## 5. State: the web UI's reducer, shared

### 5.1  What is already separable, as measured

`web/src` is closer to shareable than expected:

| Module | Depends on | Shareable as is? |
|---|---|---|
| `stores/reducer.ts` (1,615 lines, the one writer of entity state) | `api/types.ts`, `stores/resources.ts`, both type-only or pure | **Yes.** Pure `(entities, action) → entities`. No Vue, Pinia or DOM import. |
| `api/types.ts`, `stores/resources.ts` | nothing / types | **Yes.** |
| `stores/workspaces.ts`: `OVER`, `eventOutcome`, `entityOutcome`, `settles*`, `settlesJob`, `settlesClone`, `cloneOverIn` | the reducer's helpers | **The functions are pure, but the module imports Pinia** (`defineStore`, `useStreamStore`). They need moving. |
| `lib/workspaceCard.ts`, `lib/capacity.ts`, `lib/identity.ts`, `lib/portDiagnosis.ts`, `lib/time.ts` | the reducer, types | **Yes.** `lib/refetch.ts` is not: it imports Vue. |
| `api/messages.ts` (sentences per error code) | `ApiError` from `api/client.ts` | **After one move**: `ApiError` goes to core, and `client.ts` (which uses `window.location` and same-origin `fetch`) stays in web. |
| `stores/stream.ts`: the in-flight marks (`begin`, `accepted`, `end`, `snapshotTag`, the resolve-on-snapshot rule) | Pinia, `EventSource`, `setTimeout` | **No.** This is the real cost (§5.2). |

### 5.2  The split, and what it costs

A directory, **`web/src/core/`**, holds everything above marked shareable, plus two extractions:

1. **The settling predicates** move from `stores/workspaces.ts` (and `settlesRefresh` from `stores/catalog.ts`, `settlesCheck` from `stores/identity.ts`) into `core/settle.ts`. The stores re-export them, so no call site changes.
2. **The in-flight marks become a class**, `core/marks.ts`: `begin(key, settledBy, overIn)`, `accepted(key)`, `end(key)`, `snapshotTag()`, `onEvent(ev)`, `onSnapshot(entities, tag)`, with the clock injected for the 10 s *no response yet*. `stores/stream.ts` keeps the `EventSource`, the reconnect table and the refetchers, and delegates marks to it. That is about 120 lines moved, and the existing specs (`stores/inflightResync.spec.ts`, `stores/jobSettle.spec.ts`, `stores/stream.spec.ts`) stay as the proof that nothing moved but the code.

**Enforced, not hoped for.** `web/src/core/tsconfig.json` compiles core with `lib: ["ES2023"]` and no DOM library and `types: []`. `window`, `document`, `EventSource` and `localStorage` are then type errors there, and an `import 'vue'` fails to resolve. `npm run typecheck` runs it. A Vitest spec (`core/boundary.spec.ts`) also lists every import in `core/**`, fails by file on anything that is not a relative path inside `core/` (no packages, no `node:` modules), and asserts the list is non-empty. It is mutation-checked by adding `import { ref } from 'vue'` to the reducer.

**How the extension consumes it: source, by path, bundled.** The extension's esbuild bundle imports `../web/src/core/*`. There is no npm package, no publishing and no version of core separate from the commit. An npm-workspaces package (`packages/core`) is the next step if a third consumer appears, and costs only a move.

**What it costs the web UI:** one refactor PR with no behaviour change. `internal/web/dist` is rebuilt and recommitted (module paths can move chunk boundaries), `npm run check`'s budget re-measured, and `web/README.md` gains a *core* section. Specs that read Go source (`lib/capacity.spec.ts` reads `Occupying` out of `internal/workspace/state.go`) move with their modules and keep their relative paths right.

**What it buys.** The extension does not get "a similar reducer". It gets *the* reducer. Ordering by event id, idempotent replay, "only the workspace list drops a workspace", the snapshot-tag rule for marks in a `resync` gap and the fleet identity override are fixed once, for both clients, and tested once.

### 5.3  The stream in Node

`core` has no stream. The extension's `sse.ts` is about 150 lines over `fetch`:

- `GET /api/events` with `Cookie`, `Accept: text/event-stream`, and `Last-Event-ID` on a reconnect. The server honours the header, and the `?last_event_id=` query the web UI needs only because a new `EventSource` cannot send a header.
- It parses frames: `id:`, `data:`, comments (`: ping`, the 20 s heartbeat), and the one named event `resync` (`events_routes.go`'s wire-format comment). Unknown kinds go to the reducer's unknown-kind branch.
- **It classifies failures**, which `EventSource` could not (frontend §2.3). A `401` takes the one 401 path. A network error or a quiet stream (no ping for 45 s) gets backoff with the web UI's schedule (`BACKOFF_MS`). Every reopen runs the refetchers, as `stream.ts` does.
- Caddy compresses the stream (`encode`). `fetch` decodes it, and the browser tier already proves the frames are not buffered (testing §3.2's SSE row).

### 5.4  Marks, and the one mark that does not end on `workspace.job`

Every mutation the extension sends follows frontend §4.2 to the letter: begin a mark, send, discard the `202` body, and end the mark on the settling event or on a snapshot showing the action over. **Correction to the earlier sketch:** not every mark ends on `workspace.job`. A job action (start, rebuild, stop, delete, approve, supervisor) does, through `settlesJob`. **A clone does not.** The client has no workspace id until the `202` body names one, and reading that body is what §4.2 forbids. So a clone settles on the create's first `workspace.state` carrying its `repository_id` (`settlesClone`), which is *the row exists*, not *it is running*. Decline has no job and settles on the request disappearing.

So E1's *clone and follow* is two things. The mark ends quickly, and following to `running` is plain entity state: the workspace's `state` and step timeline from the reducer, shown in a `withProgress` notification until `running` (then *Connect*), `failed` (the failed step's sentence and *Open in Drydock*), or `stopped` with an `approval` (*This workspace is waiting for a host-access approval* and *Open in Drydock*, §10.4). The workspace id is learned from that same `workspace.state` event, never from the `202`. A clone of the same repository from another device settles the press too, and is followed the same way.

### 5.5  One stream per window, opened lazily

Each VS Code window has its own local extension host, and so its own stream and reducer. `SecretStorage` is shared, so one sign-in serves every window. `context.secrets.onDidChange` spreads a sign-out, and the server's own revocation ends every stream. The stream opens when the Drydock view becomes visible or a command runs, and closes after five minutes with neither and no mark in flight. Ten idle windows should not hold ten streams. Activation is `onView:drydock.*` plus the implicit `onCommand`, so a window that never touches Drydock never makes a request.

## 6. Commands, quick picks, keybindings, views

### 6.1  Commands

Every action is a command with id `drydock.<verb>`, category *Drydock*, listed in the palette. Commands take **no arguments that can cause a mutation** (§8): tree items and menus pass a workspace id only to commands that read or connect, and a mutation asks its question again.

| Command | Phase | Does |
|---|---|---|
| `drydock.signIn` / `drydock.signOut` | E1 | §4.1. Sign out is `DELETE /api/auth/session`, then the secret is cleared. |
| `drydock.go` — *Go to Workspace…* | E1 | **The hub.** One quick pick: running workspaces first (Enter connects), then other workspaces (Enter opens their actions), then repositories without a workspace (Enter clones). Same ordering and actions as the web UI's home, computed by `lib/workspaceCard.ts`'s `rowAction`/`cardStatus` from core. |
| `drydock.clone` — *Clone Repository…* | E1 | Pick a repository → `POST /api/workspaces` → follow (§5.4). At the cap, the item says *At the cap (N): stop a workspace first*, and Enter opens *Stop…* (E2) or, in E1, the web UI. Never a disabled item. |
| `drydock.connect` — *Connect to Workspace…* | E1 | Pick a running workspace → §7. |
| `drydock.refreshRepositories` | E1 | `POST /api/repos/refresh`, settled by `repo.refreshed`/`repo.refresh_failed`. |
| `drydock.openInBrowser` — *Open in Drydock* | E1 | `env.openExternal(<ui-origin>/ws/<id>)` or the home page. The escape hatch for everything the extension does not do. |
| `drydock.showLog` | E1 | The extension's own output channel (redacted, §12.8). |
| `drydock.start` / `stop` / `rebuild` / `restartSession` | E2 | Pick, confirm (§8), send. Settled by `settlesJob` and the `OVER` predicates. |
| `drydock.delete` | E2 | Pick, then an input box that must receive the repository's full name, compared exactly and sent as `?confirm=` (overall §13.5's one friction). |
| `drydock.showBuildLog` / `drydock.showSessionLog` | E2 | `GET …/build-log`, `GET …/logs?tail=n` into a read-only virtual document. |
| `drydock.checkClaudeLogin` | E2 | `POST /api/auth/claude/check`, settled by `settlesCheck`. |
| `drydock.signInToClaude` | E3 | §10.3. |
| `drydock.ports…` | E3 | List, enable, disable, open (§10.3). |

### 6.2  Keyboard: one default binding, conflicts tested

**One default keybinding**, for the hub: `ctrl+alt+shift+d` on Linux and Windows, `cmd+alt+shift+d` on macOS. One binding, because the hub reaches everything (connect is Enter on a running workspace), and every other command is in the palette for the operator to bind as they like. Two-modifier combinations near `D` are taken: `cmd+alt+d` is macOS's Dock toggle, and `ctrl+k d` is VS Code's *Compare with Saved*. The three-modifier form is the least crowded space left, but that is a claim to be checked, not trusted:

- **VS Code's defaults are checked by a test** (§12.5): the extension-host tier opens the default keybindings document (`vscode://defaultsettings/keybindings.json`) on the CI platform and fails if any default binding uses one of our key sequences. The control is that the same scan finds a known default (`ctrl+shift+p`).
- **macOS and Windows defaults, and OS-level bindings**, cannot be read on a Linux runner. They are a step in the keymap ritual (§12.10), run when the binding or the pinned VS Code version changes.
- **No `when` clause on the hub binding.** It must work in every window, including a container window, where the hub still reads and connects (§8). Tree-scoped bindings use `focusedView == drydock.workspaces`.

**The keyboard-only connect.** `ctrl+alt+shift+d` → type `alp` → the running `krelinga/alpha` is first → Enter → a new window opens attached. For a repository with no workspace, Enter clones it, the progress notification follows, and on `running` a quick pick appears: *Connect now* (default) or *Not now*. That is a quick pick rather than a notification button, because a notification needs a mouse or a focus shortcut. Setting `drydock.connectWhenReady: true` connects without asking.

### 6.3  Views and the status bar (secondary)

- **An activity-bar container, *Drydock*,** with two tree views, *Workspaces* and *Repositories*. Items carry `contextValue` (`workspace.running`, `workspace.stopped`, `repo.clonable`, …) for `view/item/context` menus, and a default command on Enter (connect, or open actions). A `viewsWelcome` shows *Sign In* when signed out and *Connect Drydock* (set `drydock.url`) before configuration.
- **A status bar item**: `Drydock: 2 running`, with the stream's phase (the web UI's quiet marker after 5 s disconnected). **The fleet identity override (frontend §6.6) lives here**: a `blanked` login is one status-bar warning, *Signed out. Sign in again.*, with one action. Ten tree items each offering *Restart session server* would be the mistake §6.6 exists to prevent. `absent` gets its own sentence, *No one has signed in yet.*
- **Never a disabled item.** An action that cannot work is replaced by the one that can (frontend §6.6): a `needs_approval` workspace shows *Review in Drydock…*, and a server outside the `api` range shows *Install the matching extension* (§9.1).

### 6.4  Settings

`drydock.url`: the UI origin. It is normalised with `new URL(x).origin` and refused unless it is `https:` with a lowercase host. The server refuses a capitalised origin at startup (overall §13.5), so a capital letter here would make every mutation `forbidden_origin`. The setting is machine-scoped (`"scope": "machine"`): it is per laptop, not synced to another device. `drydock.connectWhenReady` is the only other setting.

## 7. Connect

### 7.1  `vscode.openFolder` on the remote URI, not `openExternal` on the link

The server's link is `vscode://vscode-remote/attached-container+<hex>@ssh-remote+<host>/<path>`. VS Code's URL handler turns it into the folder URI `vscode-remote://attached-container+<hex>@ssh-remote+<host>/<path>` (`internal/vscode` package comment). From inside VS Code the extension can skip the handler and open that URI directly:

```ts
await vscode.commands.executeCommand('vscode.openFolder', remoteUri, { forceNewWindow: true })
```

| | `openFolder` (chosen) | `env.openExternal(vscode://…)` |
|---|---|---|
| Round trip | None. The same process opens the window. | Out to the OS protocol handler and back. It can land in a *different* VS Code (Insiders vs Stable, whichever registered `vscode:` last). |
| Confirmation | None. The operator pressed Enter on a named workspace. | VS Code's *Allow opening this URI?* dialog. Right for a link from the web, but a second prompt here. |
| Keyboard | Fully. | The dialog needs a click or a tab, and the OS hop can steal focus. |
| New window or this one | `forceNewWindow` or `forceReuseWindow`, our choice. | VS Code's choice. |

The extension **does not build the authority itself**. It takes the server's `url` from the view, checks its shape with core's `vscodeURL` (the same regular expression the web UI checks it with, and the shape `internal/vscode.AttachURL` produces), and rewrites only the scheme and authority prefix: `vscode://vscode-remote/` becomes `vscode-remote://`. The hex JSON, the SSH authority's encoding (`ssh-remote+<host>` or the hex `{hostName,user,port}` form) and the path's percent-encoding stay in one place, `internal/vscode`, pinned against Dev Containers 0.470.0 by its tests.

### 7.2  When there is no link

The cases are exactly the web UI's (`vscodeLink`):

- `configured: false` (no `serve --vscode-ssh-host`): *Drydock was started without `--vscode-ssh-host`, so it cannot say how VS Code reaches this server.* and *Open in Drydock*, which shows how to turn it on.
- Running, but `url` null (two containers, Docker unreadable within 3 s, no clone mount): *Drydock could not find this workspace's container just now.* and *Try again*, which refetches the view.
- Not running: Connect is not offered. *Start* (E2) or the web UI is offered instead.

### 7.3  Remote-SSH and Dev Containers: checked, not declared

`extensionDependencies` would make VS Code install both Microsoft extensions with ours and refuse ours where they cannot be installed. That is the wrong trade:

- **It turns a missing optional piece into a failed install.** On VSCodium, Cursor or any build without Microsoft's gallery, the catalog and the lifecycle would still work, but a dependency nobody can resolve blocks the whole extension.
- **It forces activation coupling** we do not need. They are needed only at the moment of *Connect*.
- **They are proprietary**, licensed for Microsoft's builds only. Declaring them makes our `.vsix` claim a dependency some users cannot legally satisfy.

Instead, *Connect* checks `vscode.extensions.getExtension('ms-vscode-remote.remote-ssh')` and `…remote-containers` first. If either is missing, Connect is replaced by *Install Remote - SSH and Dev Containers*, which runs `workbench.extensions.installExtension` for both and continues. The README states the requirement. And the SSH host in the link is the server's `--vscode-ssh-host`, which must match an entry the laptop's SSH configuration resolves. A per-laptop override is §14's open question.

## 8. The container window: commands are an attack surface

§2.3's finding sets these rules. They apply from E1, because connect is what puts the operator in a container window in the first place.

1. **A mutating command takes no meaningful arguments.** `drydock.stop(id)` called by another extension ignores `id` and shows its own pick or confirmation, so the operator, not the caller, chooses the target. Read and connect commands may take an id from a tree item, because opening a window onto a workspace the operator can already see is no escalation.
2. **Every command returns `undefined`.** Another extension host must not be able to call `drydock.*` and read back the workspace list, the repositories or a link. Tree data and state stay inside our extension host.
3. **Every mutation is confirmed by UI the operator answers.** A modal (`showWarningMessage(..., { modal: true })`) for E2's start, stop and rebuild, and the typed name for delete. Quick picks are not a confirmation by themselves: the workbench's quick-input commands (`workbench.action.acceptSelectedQuickOpenItem` and friends) are callable from any extension host, so a hostile extension can call `drydock.x` and accept its pick. A native modal has no accept command.
4. **In a container window, the extension does not mutate.** If `vscode.env.remoteName` is `attached-container` or `dev-container`, mutating commands are replaced by *Do this from a local or SSH window, or in Drydock*, with *Open in Drydock*. Reading, the hub and connect still work, since moving to another workspace is the common case there. The decision is a pure function (`mayMutate(remoteName)`), unit-tested over every `remoteName` VS Code documents.
5. **No ambient credential anywhere.** No webview ever holds the cookie or makes Drydock requests. All HTTP comes from one client module in the extension host, and it sends the cookie only to `drydock.url`'s origin, after validating the path against the route patterns it knows. It never follows a redirect to another origin with the cookie attached.

Rules 1 to 3 are tested by a hostile fixture extension in the extension-host tier (§12.8). Rule 4 is tested by its unit table and by the adapter test that every mutating command consults it.

## 9. Version skew and release

### 9.1  The handshake: `GET /api/meta`

A new route, **`meta.read`**: `GET /api/meta`, `AuthRequired`, not mutating.

```json
{ "version": "v0.12.0", "api": 1 }
```

- `version` is `main.version`, passed into the server config (`dev` for local builds).
- `api` is an integer the server bumps when a route, event kind or view field **the extension uses** changes incompatibly. Additive changes (a new route, a new event kind, a new field) do not bump it: the reducer's unknown-kind branch and ignored unknown fields are what keep the old client working.
- **Authenticated, on purpose.** The version is information about the server for someone who can use it, and an unauthenticated route would be a second `AuthNone` on the API mux, which the meta-test forbids. The extension asks right after sign-in, and again on every stream reopen, since the server may have been upgraded underneath it.
- **An older server answers `404 not_found`** (authenticated, through `api.Unrouted`) and is read as `api: 0`.

The extension declares `drydock.apiRange: [min, max]` in its own `package.json`. The decision is a pure function, unit-tested over the whole table:

| Server `api` | Extension does |
|---|---|
| within `[min, max]` | Everything. If `version` differs from the extension's own, a quiet one-time notice offers the matching `.vsix`. |
| `< min` (including `0`, a pre-`meta` server) | Reads only: the lists, follow, and connect, which use routes that have not changed since Phase 2. Mutations are replaced by *This Drydock is older than this extension supports. Upgrade Drydock, or install the extension from its release.* |
| `> max` | The same, with *This extension is older than the Drydock it is talking to* and *Install the matching extension* as the replacement action. |

**The contract is pinned by a test, not by memory.** `extension/contract.json` lists every route name the extension calls (with method and pattern), every event kind it reads, and the view fields it uses. A Go test (`internal/api`, `TestExtensionContract`) fails when a listed route is missing from `api.Table` or has a different method or pattern. Event kinds are checked against the `Kind*` string constants found by the same AST scan `internal/events`' `TestRowRule` uses. The failure message says *bump `api` in `internal/api/meta.go` or keep the route*. It is mutation-checked by renaming `workspaces.create`'s pattern.

### 9.2  Release: a `.vsix` on every Drydock release

The extension lives in **`extension/`** (beside `web/` and `feature/`) and is versioned with Drydock: one tag, one version, and *the extension that matches* is unambiguous.

- **Version stamping.** release-please is `release-type: go` with one root package. It does not touch `package.json`. `release-please-config.json` gains `"extra-files": [{"type": "json", "path": "extension/package.json", "jsonpath": "$.version"}]`, so the release PR bumps it. A `test/docs` check fails when `extension/package.json`'s version differs from `.release-please-manifest.json`'s.
- **Assets.** The `assets` job builds the bundle (esbuild) and packages it with `npx @vscode/vsce package --no-dependencies -o dist/drydock-vscode.vsix`, a **fixed name** like the others, so `releases/latest/download/drydock-vscode.vsix` always resolves. `SHA256SUMS` covers it.
- **`verify` changes in the same PR.** It asserts the exact asset list (`SHA256SUMS drydock_linux_amd64.tar.gz install.sh`). That line gains `drydock-vscode.vsix`. It also checks that the `.vsix`'s `extension/package.json` says the tag's version (`unzip -p … | jq -r .version`), and runs the extension-host tier's smoke spec against the downloaded `.vsix`, so what is published is what was tested.
- **Installing and updating.** First install: *Extensions: Install from VSIX…*, or `code --install-extension drydock-vscode.vsix`. VS Code does not auto-update a sideloaded extension, so the handshake's notice does: *Install the extension for Drydock v0.12.0* downloads `releases/download/<version>/drydock-vscode.vsix` and `SHA256SUMS` from GitHub, checks the hash, and runs `workbench.extensions.installExtension` on the file. The hash comes from the same origin, so it guards against corruption, not against a compromised release, and the doc says no more for it than that.

**Why not the Marketplace, yet.** A listing needs a publisher identity and a publishing token in CI (a new secret, and a new party able to ship code to the operator's editor). It invites users the design does not serve: one operator, one server (overall §1). And it would auto-update the extension *ahead of* the server, which is skew by default. Revisit if anyone but the owner runs Drydock. Open VSX does not help: §2.4's dependencies are not there.

## 10. Phasing

Each phase states when it is done, as overall §14 does.

### 10.1  E1: sign in, catalog, clone, follow, connect

`signIn`, `signOut`, `go`, `clone`, `connect`, `refreshRepositories`, `openInBrowser`, `showLog`. The tree views, status bar and handshake. Server changes: `GET /api/meta` and the device label (§11). The `core/` split lands first, as its own PR.

**Done when:** from a fresh VS Code on the laptop, with the keyboard only, the operator signs in, clones a testbed repository, watches it reach `running`, and lands in a new window attached to its container at the clone's folder. The connect ritual (§12.10) has passed once. The extension-host tier passes in CI.

### 10.2  E2: the lifecycle

`start`, `stop`, `rebuild`, `restartSession`, `delete` (typed name), the two logs, `checkClaudeLogin`, the fleet identity override in the status bar, and *Make room* at the cap. Every mutation follows §8.

**Done when:** each command's in-flight mark ends on its settling event in the extension-host tier, including after a forced `resync`. The hostile-extension test (§12.8) shows none of them can be driven without the operator.

### 10.3  E3: the Claude login and ports

The login handshake (frontend §6.2): `POST /api/auth/claude/login`, show the authorize URL (`env.openExternal` to open it), and take the code in `showInputBox({ password: true })`. A login code is a credential, so it is never shown back and never logged, and it is a canary in the sweep. Ports: list, enable, disable, open. Opening a preview goes through the browser's own preview handshake (`/preview/authorize`), which needs the *browser's* session. The extension's cookie never reaches a browser.

**Before E3: option (b) tokens**, with a scope for the extension that includes the login and ports but not approvals or secrets (§4.5).

### 10.4  Approvals and secrets: web-only, and why

**Host-access approval is the most security-sensitive surface Drydock has.** Approving `privileged` gives a repository host root (overall §13.4), and frontend §6.5a spends a section making sure the button cannot be pressed without reading. An editor is the worst place to read it. The approval is asked for because of a change *in the repository the operator has open in that same editor*, possibly made by the agent, and the reflex is to approve and get back to work.

**Recommendation: the extension never approves.** A `needs_approval` workspace shows *Review the host-access request in Drydock* (`openInBrowser` to `/ws/<id>`) where its action would be. With option (b), the extension's token scope leaves out `workspaces.approve`, so that is a server property, tested by the scope meta-test.

If the owner decides otherwise, the extension's version must keep everything frontend §6.5a requires, and more:

- **The full subset, always**: new, changed (both values) and no longer asked for, each with its source, as JSON, in a read-only virtual document the operator must have open. Never a summary line in a notification.
- **The warning** in the modal's own text: *root on the host* whenever `privileged` is in the request.
- **Never one keystroke.** No keybinding, no quick-pick item that approves, no notification button. A native modal whose default button is *Cancel* (VS Code always gives a modal a *Cancel*), opened only from the document's own action.
- **The hash shown is the hash sent**, and `409 approval_stale` re-opens the document on the new request.
- **Never from a container window** (§8, rule 4), and only with a token scoped to include it.

**Secrets stay web-only for a simpler reason:** the value would be typed into the editor, where clipboard managers, extensions that observe input, and settings sync are all nearby. The web UI's form is the place this repository has made safe (frontend §6.4). Reading secret *metadata* in the extension (names, reach, grants) is harmless, but it is also not useful enough to justify the scope.

## 11. Required server changes

| Change | Phase | Where |
|---|---|---|
| `GET /api/meta` → `{version, api}`, `AuthRequired`. `version` threaded from `main.version` into `config.Config`. | E1 | `internal/api` route table + handler, `cmd/drydock`, overall §5 |
| `TestExtensionContract` over `extension/contract.json` | E1 | `internal/api` |
| `deviceLabel` recognises a `Drydock-VSCode/<version>` user-agent token → *VS Code on Mac*. The extension sends `User-Agent: Drydock-VSCode/<v> (<platform>)`. | E1 | `internal/api/session_routes.go` |
| Release: `extra-files`, the `.vsix` asset, `verify`'s list | E1 | `release-please-config.json`, `.github/workflows/release-please.yml` |
| Scoped API tokens: schema row, mint/revoke routes, `Authenticate` with a bearer, a `Scope` column in the route table, its meta-test, the Settings UI, the CLAUDE.md invariant wording | before E3 | `internal/auth`, `internal/api`, `internal/store`, `web/`, overall §4 and §13.2 |

Nothing in E1 or E2 needs a new mutating route, a new event or a change to an existing response.

## 12. Testing

The owner's worry is right: IDE extensions are usually tested by a UI robot clicking through a real editor, and that is slow, flaky and mostly tests the editor. This plan follows testing §3.1 instead: **put each assertion in the cheapest tier whose reach includes the boundary it is about**. For this extension most boundaries are not the editor. They are the HTTP protocol, the reducer, the manifest, and the one call that hands VS Code a URI. A UI robot is left with the single thing only it can see.

### 12.1  The tiers

| Tier | Real | Substituted | Lives in | Budget |
|---|---|---|---|---|
| **unit** | The pure modules: command logic over ports, the SSE parser, `Set-Cookie` extraction, link → URI, setting validation, the `api` range decision, `mayMutate` | `vscode`, by not being imported. HTTP, by a fake transport. | `extension/src/**/*.spec.ts` (Vitest, node) | < 10 s |
| **core** | The shared reducer and marks | As today | `web/src/core/**/*.spec.ts`, run by web's `npm run check` | unchanged |
| **manifest** | `extension/package.json` against the source's command registry | Nothing to substitute | `extension/test/manifest.spec.ts` (Vitest) | < 1 s |
| **client** | The extension's HTTP client and SSE reader (no VS Code) against **real `drydock` behind real Caddy** on the shipped Caddyfile, with a test CA | GitHub (`githubtest`), the devcontainer CLI and Docker (the `internal/server` seams), Claude (`fakeclaude`) | `test/extension/client/*.spec.ts` (Vitest, node) | < 60 s |
| **extension host** | A pinned VS Code, headless under xvfb, with the **packaged `.vsix`** installed, against the same stack | Remote-SSH and Dev Containers (absent). `vscode.openFolder` is observed, not performed. | `test/extension/host/` (`@vscode/test-cli`) | < 5 min |
| **keyboard smoke** | Real keystrokes into a real VS Code window | As above | `test/extension/ui/` (`wdio-vscode-service`) | nightly, non-gating |
| **rituals** | Everything: the laptop, Remote-SSH, Dev Containers, the real server | Nothing | `test/rituals/vscode-*.md` | by event |

### 12.2  Unit, and how the `vscode` boundary is enforced

The extension has two layers. **`src/adapter/`** is the only code that imports `vscode`: registering commands, the quick-pick and input-box wrappers, `SecretStorage`, tree providers, `openFolder`. It is thin enough to read in one sitting (a target of ~400 lines), with no branches worth a unit test. **Everything else** is pure and talks to the adapter through narrow interfaces (`Prompter`, `Secrets`, `Opener`, `Clock`, `Transport`).

Three things make the boundary hold:

1. **Compilation.** `src/tsconfig.pure.json` covers everything but `adapter/` with `types: []`, so `@types/vscode` is invisible there and `import * as vscode from 'vscode'` is a compile error. `npm run check` runs it.
2. **Execution.** The `vscode` module does not exist outside the editor (`@types/vscode` is types only), so a pure module that reached it would fail its own Vitest spec at import.
3. **A named rule.** `boundary.spec.ts` lists every import of every non-adapter file and fails by file on `vscode`, `node:child_process` (the extension spawns nothing, so there is no argv sink to sweep), or any `rejectUnauthorized` or `NODE_TLS_REJECT_UNAUTHORIZED` text anywhere in `src/`. It asserts the scan saw more than zero files and found the adapter's `vscode` imports, which is the positive control. Mutation-checked by adding a `vscode` import to `src/link.ts`.

Specs worth naming:

- **SSE parser**: frames split across chunks, a CRLF stream, a comment-only stream (heartbeat), a `resync` with its id, an unknown kind passing through, and a `data:` split over two lines. Fed the server's real output recorded from the client tier, as testing §7 records Claude's.
- **`Set-Cookie`**: takes exactly `__Host-drydock`, ignores another cookie in the same response, and treats `Max-Age=0` as sign-out.
- **Link → URI**: every case `internal/vscode`'s tests pin (bare host, user, port, capitals, encoded path), read from **the Go test's own table**, as `lib/capacity.spec.ts` reads `Occupying`. Every shape the web's `vscodeURL` refuses, the extension refuses too.
- **Command logic over a fake `Prompter`**: the clone flow's every exit (cancelled pick, `409 in_progress`, `409 at_capacity` with its detail, `507 disk_full`, `503 app_not_configured`), each producing the sentence `api/messages.ts` gives the web UI.

### 12.3  The manifest is a tested artifact

Most "does the palette and keyboard work" questions are questions about `package.json`, and they need no editor:

| Assertion | Control |
|---|---|
| Every `contributes.commands` id is `drydock.*`, has category *Drydock*, and is registered by `activate` (the source's command registry list, which the adapter iterates) | The registry and manifest sets are equal and non-empty |
| Every command is in the palette: none is hidden by `menus.commandPalette` `when: false` unless an allowlist names it with a reason | One allowlisted entry exists, so the allowlist path is live |
| Every keybinding names a contributed command, has `mac` and default keys, and its `when` parses (VS Code's `when` grammar, a small parser in the spec) and uses only context keys the source sets or VS Code documents | A deliberately broken fixture manifest fails each check by name |
| `extensionKind` is exactly `["ui"]`. No `extensionDependencies`, no `extensionPack`. `activationEvents` contains no `*` and no `onStartupFinished`. | |
| `engines.vscode` equals the pinned test version's minor (`^1.N.0`) | |
| No `contributes.configuration` key is `application`/`window`-scoped where §6.4 says `machine` | |

Mutation-checked by deleting a command from the palette, adding `*` activation, and adding `extensionDependencies`. At runtime the extension-host tier adds the one check the manifest cannot: `vscode.commands.getCommands(true)` contains every contributed id once the packaged extension has activated.

### 12.4  The client tier: the protocol against the real server, without VS Code

This is the tier that does the most work, and it is cheap because it is plain Node.

**The stack.** It reuses the browser tier's harness rather than copying it. `test/browser/harness.ts`'s certificate minting, Caddy process and per-run temp root move to `test/harness/` and are shared by both tiers. The server differs. The browser tier runs `drydock serve` with no GitHub App, so `GET /api/repos` is `app_not_configured`. E1 needs a catalog and a clone that reach `running`. So the stack runs **`test/extension/stack`**, a small Go program under a build tag (`extensiontier`, never in `deploy/package.sh`'s build, as `browsertier` is not). It calls `server.New` with the same seams the walking-skeleton test uses (`internal/server/workspace_test.go`): `githubtest` with `EnableGit` as the App and the git remote, `Provisioner.Containers.Run` as a fake devcontainer that reports a running container, and a `vscode.Containers` that reports that container's name and clone mount. It listens on the real Unix socket, behind the real Caddy on `deploy/Caddyfile`. A positive control in the stack's own test proves the fakes are wired: the catalog lists the seeded repositories, and a clone's view carries a link.

**Trust without turning validation off.** The client tier runs Vitest with `NODE_EXTRA_CA_CERTS=<root>/ca.pem`. Node adds that CA to its default root store at startup, for `https` and `fetch` alike, with validation fully on. The browser tier's rule carries over: never `rejectUnauthorized: false`, never `NODE_TLS_REJECT_UNAUTHORIZED=0`. The control is the browser tier's wrong-host certificate: the same client pointed at a Caddy serving a valid-chain certificate for another name must fail with a TLS error and send nothing (the tap shows no request).

**Name resolution.** The UI origin is `https://drydock.test:<port>`, and the stack's `--ui-origin` is set to match, since `OriginAllowed` compares the port too. In CI the job adds `127.0.0.1 drydock.test` to `/etc/hosts` as root before dropping to `pwuser`. Locally `run.sh` refuses with a one-line instruction rather than editing `/etc/hosts` itself. No resolver hook is added to the production client.

**What it asserts**, each with its control in the same test:

| Assertion | Control |
|---|---|
| A mutation without `Origin` is `403 forbidden_origin`. The client always sends it, so this is the test that the client's header is what makes it pass. | The same client's `POST /api/workspaces` → `202` |
| The cookie is sent only to the UI origin: a `3xx` to another origin is not followed with it (a tap on a second Caddy site records no `Cookie`) | The same request to the UI origin carries it (tap) |
| Sign-in → cookie stored. `SignInNotice` surfaced after seeded failures. `429 locked_out` → `Retry-After` honoured, no retry | Correct password after the window → `204` |
| A `401` mid-stream (*Sign out everywhere* from a second client) → the stream ends within one heartbeat, the client's one 401 path runs, the stored cookie is cleared | Before the sign-out the stream delivered events |
| SSE reconnect with `Last-Event-ID`: kill Caddy, restart it, and events written meanwhile arrive once each, in order. Past the replay window → `resync` → refetch | Events before the kill arrived |
| **Clone and follow, end to end**: `POST /api/workspaces` → the mark ends on `settlesClone` → the reducer reaches `running` → the refetched view carries a `vscode.url` that `vscodeURL` accepts | |
| `GET /api/meta` answers the stack's `{version, api}` | |

The fake transport in the unit tier is checked against this tier: the recorded responses it replays are captured here, so the fake can never claim a shape the server does not produce (testing §6.1, *no fake without a contract test*).

### 12.5  The extension-host tier: commands in a real VS Code

`@vscode/test-cli` (with `@vscode/test-electron`) downloads a **pinned** VS Code version, cached in CI and checked against a SHA-256 in the repo before it is unpacked (the same treatment CI gives `certutil`'s `.deb`). It runs headless under `xvfb-run`. Each run gets its own `--user-data-dir`, `--extensions-dir` and `--logs-path` under the run's temp root, so the canary sweep (§12.8) has one tree to search. It runs with `--password-store=basic`, since CI has no keyring.

**It tests the `.vsix`, not the source tree.** `npm run package` builds the same artifact the release uploads, and the test installs it into the extensions directory. That makes `extensionMode` `Production`, as on the laptop. A second, smaller run uses `--extensionDevelopmentPath` and `ExtensionMode.Test` for the specs that need the test seam below.

**Driving commands.** Specs call `vscode.commands.executeCommand('drydock.clone')` and so on. The test seam is narrow and observable:

- In `ExtensionMode.Test` **only**, `activate` returns `{ prompter, opener }`. The first is a scripted `Prompter` answering picks and inputs, and the second is a recording `Opener` that stores the URI it would hand `vscode.openFolder` instead of opening a window. Nothing in the dev host can resolve `ssh-remote+…` (§12.10).
- **The control for the seam itself:** with the packaged `.vsix` in `Production` mode, `vscode.extensions.getExtension('krelinga.drydock').exports` is `undefined`. The seam cannot ship live. Mutation-checked by returning the exports unconditionally.
- The real `Prompter` and `Opener` (each a few lines over `showQuickPick` and `executeCommand('vscode.openFolder', …)`) are covered by the keyboard smoke test and the connect ritual, and by a unit test that pins the exact call: command id, URI and `{ forceNewWindow: true }`.

**What it asserts:**

- **Activation is lazy**: after the window opens and no Drydock view is shown, the tap has seen no request. Control: opening the view causes one `GET /api/workspaces`.
- **Sign-in persists across a reload** (`workbench.action.reloadWindow`, then the extension host comes back signed in). That proves the cookie went to `SecretStorage`. §12.8's sweep proves it went nowhere else.
- **E1's flow from the hub**: `drydock.go` → scripted pick of an unheld repository → clone → follow → the recorded `Opener` URI equals the `vscode-remote://` rewrite of the view's `url`, byte for byte.
- **Every E2 mark ends on its settling event**, including after the stack forces a `resync` (an event-log gap past the replay window). This is the extension's copy of `inflightResync.spec.ts`, run against the real server.
- **Keybinding conflicts**: open `vscode://defaultsettings/keybindings.json` and parse it. No default binding uses `ctrl+alt+shift+d`. Control: `ctrl+shift+p` is found as `workbench.action.showCommands`.
- **Version skew** (§12.7).

### 12.6  UI-level tests: evaluated, and kept small

| Option | What it is | For | Against |
|---|---|---|---|
| **ExTester** (`vscode-extension-tester`) | Selenium + ChromeDriver against VS Code's Electron, with page objects for the workbench | Mature page objects for quick picks, trees, notifications | Its own VS Code and ChromeDriver downloads per version, page objects that break when VS Code's DOM changes (monthly), Selenium flakiness, minutes per spec |
| **`wdio-vscode-service`** | WebdriverIO service that launches VS Code and exposes workbench page objects | Same reach, a lighter runner, real keystrokes, and it can run the extension-host API from the test | The same DOM churn and download cost |
| **Manifest contract + extension-host tier** (§12.3, §12.5) | No DOM | Covers registration, palette presence, `when` syntax, conflicts with defaults, every command's behaviour, the exact URI | Cannot see a keystroke reach a command, or a quick pick render |

The contract tier and the extension-host tier cover nearly everything a UI robot would, at a fraction of the cost and with no DOM to drift. What only a robot can see is **the keyboard path itself**: the chord reaches `drydock.go`, typing filters, Enter on the first item connects. So there is **one** `wdio-vscode-service` spec (chosen over ExTester for the lighter runner and real key events): *keyboard-only connect* against the stack, ending at the recording `Opener`. It runs **nightly**, beside the stress job, and on demand, and is **not a required check**. A flake there files an issue the way the stress job does (`flake: extension-ui …`). It is promoted to CI only after a month without a flake. If it never gets there, the connect ritual covers the same path by hand.

### 12.7  Version skew, both directions

- **Unit**: the `api`-range decision over every row of §9.1's table.
- **Newer server, current extension**: the stack is started with `meta.API = max + 1` (a field on the handler struct the stack sets, with no build-time constant to override). The extension-host tier asserts every mutating command is replaced by *Install the matching extension* and sends nothing (tap). Read commands and connect still work, which is the control.
- **Older server, current extension**: the client and extension-host tiers run once against the **previous release's binary**: downloaded by tag, checked against that release's `SHA256SUMS`, and started by the stack harness in place of the stack program (the browser tier's `drydock serve` path). It has no `/api/meta`, so `404` → `api 0` → read-only. The control is that the lists still load from the old server.
- **Older extension, current server**: once a release carries a `.vsix`, the extension-host tier also installs **the previous release's `.vsix`** and runs its smoke spec against today's stack. It must either work fully (within range) or show the update action, and never fail in some third way. This is what checks the server's claim that an additive change did not need an `api` bump.
- **Contract drift**: `TestExtensionContract` (§9.1), in the ordinary `go test`.

### 12.8  Security tests

**The canary sweep, extended to the editor** (testing §4.2). The extension-host run seeds a high-entropy password canary and sets the stack's password to it. The session cookie is minted by the server, so its value is captured from the Caddy→socket tap as the canary. After a full E1 scenario (sign in, clone, connect, sign out, sign in), the sweep greps the whole run root (user data dir, extensions dir, logs, `globalStorage`, `workspaceStorage`, crash dumps, captured stdout and stderr, and the VS Code output channel's log files) for each canary and its base64, URL-encoded and JSON-escaped forms:

- **Expected: no hit** for the password anywhere, and no plaintext cookie anywhere. With `--password-store=basic`, `SecretStorage` keeps an encrypted blob in `state.vscdb`, which the plaintext and encoded searches do not match. The test does not pretend `basic` is strong (§4.1).
- **The sweep must find something**: a non-secret marker (the cloned workspace's id), which the extension logs at info level, is found by the same sweep in the output channel's log. Without it, a sweep that searched an empty tree would pass.
- E3 adds the Claude login code, and option (b) the API token, as canaries.

**The hostile extension** (§8). A fixture extension, `test/extension/host/fixtures/hostile/`, is installed beside ours. It calls every `drydock.*` command with crafted arguments (a workspace id, `{ confirm: "krelinga/alpha" }`), then immediately calls `workbench.action.acceptSelectedQuickOpenItem`. Asserted at the tap: **no mutating request reached the server**, and every `executeCommand` returned `undefined`. The control in the same test is that the scripted `Prompter` (the operator, in effect) completing the same command *does* produce the `POST`. Mutation-checked by letting `drydock.stop` honour its argument.

**TLS and `Origin`**: the wrong-host certificate and the `Origin` pair live in the client tier (§12.4). The extension-host tier repeats the wrong-host case once with VS Code's own HTTP stack in play, since VS Code patches Node's `https` and `fetch` for proxies and system certificates. That run also asserts `http.proxyStrictSSL` is not `false` in the test profile, since a test profile that disabled it would quietly pass the wrong-host case.

**Open question to settle in the spike:** that `NODE_EXTRA_CA_CERTS` reaches the extension host process. It is inherited through the environment `@vscode/test-electron` passes to VS Code, and VS Code is known to honour it for corporate CAs, but this has not been measured here. If it does not, the fallback that keeps validation on is the system store: install the test CA into the CI container's `/usr/local/share/ca-certificates` before dropping privileges, and rely on VS Code's `http.systemCertificates` (default on). The first task of E1's test work is a spike that tries both, records which applies, and pairs each with the wrong-host control. It is filed as Spike 05 with a re-runnable harness, as the repository's spikes are.

### 12.9  Invariant → test map

| Invariant | Tier | Test | Control |
|---|---|---|---|
| The extension invents no state: entities come only from the reducer | core + unit | The extension has no entity setter. `boundary.spec.ts` fails on any write to `entities` outside core | Reducer specs move state |
| A mark ends on its settling event, never the `202` | extension host | §12.5, with a forced `resync` | The mark exists before the event |
| `Origin` is the UI origin, and nothing else gets it | client | §12.4 | `202` with it |
| The cookie only to the UI origin; no ambient credential | client + extension host | Cross-origin redirect tap; no webview in the manifest (§12.3) | Same-origin request carries it |
| Password, cookie (and later login code, token) never written outside `SecretStorage` | extension host | Sweep | Sweep found its marker |
| TLS validation never off | unit + client + extension host | `boundary.spec.ts` text rule; wrong-host cert refused | Right-host cert accepted |
| Commands cannot be driven by another extension | extension host | Hostile fixture | Scripted operator drives the same command |
| No mutation from a container window | unit + adapter test | `mayMutate` table; every mutating command consults it | Local and SSH remote names may mutate |
| Lazy activation, no request without use | extension host | Tap empty after start | View open → request |
| The link is the server's, rewritten only in scheme | unit + extension host | Go table cases; recorded `Opener` URI | |
| Skew is detected in both directions | client + extension host | §12.7 | In-range server works fully |
| The release ships what was tested | release `verify` | Smoke spec against the downloaded `.vsix` | `jq .version` equals the tag |

### 12.10  What CI cannot test, and the rituals that do

**The real attach cannot run in CI.** Remote-SSH and Dev Containers are proprietary and not installable in an automated test profile without accepting their licence for a CI machine. They need a real SSH host with Docker. And the window they open cannot be observed by the extension that asked for it. So CI stops at the boundary and asserts it exactly: the URI and the command handed to VS Code (§12.5). Everything past that is a ritual, a file in `test/rituals/` with a checklist and commands (testing §11):

- **`vscode-connect.md`** — the E1 done-when on the real laptop and server: install the release's `.vsix`, sign in, and, with the keyboard only, clone `krelinga/drydock-testbed-a`, follow it, connect. In the new window: the folder is the clone, `git push` of a `drydock/` branch succeeds through the broker, *Reopen in Container* of the clone on the host finds Drydock's container rather than building one (design §6), and the device list shows *VS Code on Mac*. Then from that container window, a mutating command says to do it elsewhere (§8, rule 4). Run before each release that changes `extension/`, and as part of the release smoke test (testing §11.4).
- **`vscode-bump.md`** — triggered by a bump of the pinned VS Code or when Remote-SSH or Dev Containers update past the version `internal/vscode` was read from (0.470.0): re-read the attached-container authority in the Dev Containers extension (the procedure `internal/vscode`'s comment describes), re-run the connect ritual, and move the pins in the same commit.
- **`vscode-keymap.md`** — triggered by changing a default keybinding or bumping the pinned VS Code: on macOS and Windows, open *Keyboard Shortcuts*, search the chord, and confirm only Drydock holds it. Check the OS's own shortcut list too.

### 12.11  CI and the nightly run

- **`go` job**: `TestExtensionContract` and the stack program's own test run in the ordinary `go test ./...`. The stack builds under its tag inside the client-tier script, not in `go-suite`.
- **`web` job**: unchanged. `npm run check` now covers `core/` and its boundary spec.
- **A new `extension` job** in `.github/workflows/ci.yml`, in **the same Playwright image the `browser` job uses**, pinned by the same digest. It already carries Node, Xvfb and Chromium's system libraries, which Electron's overlap heavily, and apt on the bare runner is what drove the browser job into that image. The job adds Go and the pinned Caddy as the browser job does, writes the `/etc/hosts` line and (if the spike says so) the CA, then runs as `pwuser` via `setpriv`: `extension/`'s `npm ci && npm run check` (types, unit, manifest, boundary, bundle budget), then `test/extension/run.sh` (the client tier, the `.vsix` build, the extension-host tier). The VS Code download is cached by version. **Not a required check for its first month**, then added to the ruleset, the same probation as the keyboard smoke test.
- **Nightly**: the keyboard smoke spec runs after the stress job's loop in `stress.yml`, filing `flake: extension-ui <spec>` issues the same way. Once the extension-host tier is required, its specs join the stress loop's "every other package" share. A timer-driven SSE client is exactly the kind of code that job exists to shake.
- **Release**: `test` is unchanged. `assets` builds the `.vsix`. `verify` checks it and runs the smoke spec against the downloaded artifact (§9.2).

## 13. What this deliberately gives up

- **No reach beyond the LAN.** The extension reaches Drydock where the browser does (§2.1).
- **No auto-update** without the operator pressing *Install*. Skew is detected and named instead (§9).
- **No approvals, no secrets** in the extension, by recommendation (§10.4).
- **No proof in CI that a window actually attaches.** The URI handed over is proven exactly, and the attach is a ritual (§12.10).
- **No webview UI.** Quick picks, trees, notifications and virtual documents only. That costs some presentation (the step timeline is a progress notification and a tree, not the web UI's timeline) and buys §8's *no ambient credential*.

## 14. Open questions

1. **Auth: is (a) acceptable for E1 and E2**, with (b) scoped tokens landing before E3 (§4.5)? Or should (b) come first, so the extension never holds a full session at all?
2. **Approvals and secrets: confirm they stay web-only** (§10.4), with the extension's token scope enforcing it from E3. Or name the conditions under which the extension may approve.
3. **Container windows**: is "no mutations from a container window" (§8, rule 4) the right line, or too strict? The cost: stopping workspace B from inside workspace A's window means the palette says *do this from a local or SSH window*.
4. **The default keybinding**: `ctrl+alt+shift+d` / `cmd+alt+shift+d` for the hub, and no others (§6.2). Or a different chord, or none at all?
5. **Where the `.vsix` comes from for updates**: GitHub releases (§9.2), or **the Drydock server serving its own `.vsix`** at an authenticated route, which removes GitHub from the update path and can never be mismatched. That would grow the binary by a few hundred KB and add a route.
6. **The SSH host per laptop**: the link uses the server's `--vscode-ssh-host`. Should the extension allow a per-laptop override of the SSH authority (a second laptop with a different SSH alias), at the cost of re-encoding the authority in TypeScript, which today lives only in `internal/vscode`?
7. **Off-LAN use**: does the owner ever work over SSH from outside the LAN? If so, a relay (a workspace-side companion that forwards the extension's HTTPS to Caddy from the server itself, over the Remote-SSH connection) is the next design question. It is not in any phase above.
8. **A `drydock` CLI client**: worth building on its own for scripts and terminals? It would share the client tier's protocol tests, but it is not the extension's engine (§3.3).
9. **One Drydock or several**: E1 assumes one `drydock.url`. A second Drydock (a test instance) would need a server picker, per-server secrets and per-server streams.
