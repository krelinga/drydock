# Drydock for VS Code

*A VS Code extension that drives Drydock from the command palette: list the repositories, clone one, follow it to running and open it in a new window, all without leaving the editor or touching the mouse. It is a second client of the same API, so it inherits the web UI's rules rather than inventing its own: every mutation is a `202` and a wait, and the event stream is the only thing that changes what it shows.*

**Status** living design document. Its history is the git log (`git log -p -- docs/design/vscode-extension/vscode-extension-design.md`). Phases R (the event-kind registry) and T (API tokens, server and Settings) are built; the extension itself is not.

**Supplements** [`../overall/drydock-design.md`](../overall/drydock-design.md) (§5 the API, §6 *Opening a workspace in VS Code*, §13 security) · [`../frontend/frontend-design.md`](../frontend/frontend-design.md) (§2.1, §4, §6.5a, §6.6) · [`../testing/testing-design.md`](../testing/testing-design.md)

**Runtime** the operator's own VS Code, on the laptop (`extensionKind: ["ui"]`), talking HTTPS to Caddy at the UI origin

**Reach** one operator, one Drydock, wherever its UI origin resolves: the LAN, or the owner's tailnet (§2.1). The same reach as the web UI

**Out of scope** a session browser · a terminal or log viewer beyond an output channel · VS Code forks that cannot run Microsoft's Remote extensions · more than one Drydock (decided: one `drydock.url`) · a standalone CLI client (decided: not now)

## Decisions

| | Decision | Where |
|---|---|---|
| Where it runs | The laptop's extension host (`extensionKind: ["ui"]`). It works before any remote window exists and can open new windows. | §3.1 |
| How it reaches Drydock | HTTPS to Caddy at the UI origin, like a browser. Never the Unix socket. | §3.2 |
| Auth | **Scoped, revocable API tokens, built first** (owner's decision): a server phase, **T**, before E1. The operator mints a token in the web UI's Settings, and the extension stores it in `SecretStorage`, bound to the origin it was used against. The extension never holds a session cookie or sees the password. Scopes are `read`, `lifecycle` and an explicit **`approve`** (default off, covering approve and decline). Secrets, tokens, sessions and *Sign out everywhere* are never in a token's scope. Bearer requests pass the same gate in the same order, plus one scope step, and still send the exact `Origin`. | §4 |
| Settings | `drydock.url` is **`application`-scoped with `ignoreSync`**: user settings on the laptop only, never taken from a remote's or a workspace's settings, which a repository can write. The stored secret is `{origin, token}` and is sent to that origin alone, so changing the URL means signing in again; the old token is sent nowhere meanwhile, and the operator is asked whether to sign out of the old Drydock (revoke and forget) or keep it. | §6.4 |
| `Origin` | The extension sends the literal UI origin on every mutation. This forges nothing: the check defends browsers against third-party pages, and a client that already holds the credential was never inside its threat model. The server does not change. | §4.3 |
| State | **The web UI's reducer, unchanged, shared as source** from a DOM-free `web/src/core/`. The extension follows "the frontend invents no state" with the same code. | §5 |
| Commands | Every action is a `drydock.*` command in the palette. A single *Go to Workspace…* quick pick is the hub. One default keybinding, and a test checks it for conflicts. A tree view is the secondary surface. | §6 |
| Connect | `vscode.openFolder` on the `vscode-remote://` form of the server's own link, in a new window. Not `openExternal` on the `vscode://` link. Remote-SSH and Dev Containers are checked for at connect time, **not** declared as `extensionDependencies`. | §7 |
| Commands as attack surface | Code in an attached container can run commands in that window, ours included. **The primary control: outside an allowlist of window kinds (local, `ssh-remote`), the extension does not mutate at all.** Depth: commands ignore arguments that could cause a mutation, return nothing, and E2's lifecycle mutations confirm in a modal. The extension cannot make a container window safer than the attach itself (§8.2). | §8 |
| Version skew | A new authenticated `GET /api/meta` returns `{version, api}`. The extension declares the `api` range it speaks. A Go test pins the routes and event kinds the extension uses, the kinds against a new **event-kind registry** in `internal/events` (§9.1). The client tier checks the event kinds it reads were actually emitted. Outside the range, a mutation is replaced by *Install the matching extension*. | §9.1 |
| Release | A `.vsix` uploaded to every Drydock release under a fixed name (`drydock-vscode.vsix`) by the existing release-please flow. Not the Marketplace, at least at first. | §9.2 |
| Approvals | **In E1** (the owner's decision: docker-in-docker repositories stop at `needs_approval` right after clone, so clone → connect dead-ends without it). They come from a read-only **review document** showing exactly what the web card shows, and are confirmed by **typing the repository's full name**. They are allowed only from local and SSH windows. The approval sends the hash of the request that document rendered, never a hash it did not show. | §10.4 |
| Phasing | **R** (server, a small PR first): the event-kind registry. **T** (server): API tokens, scopes, `GET /api/meta`. **E1**: sign in with a token, catalog, clone, follow, **host-access approval**, connect. **E2**: start, stop, rebuild, delete, logs, Claude status. **E3**: the Claude login, ports. **Secrets stay in the web UI.** | §10 |
| Settled by the owner | One default keybinding, `ctrl+alt+shift+d` / `cmd+alt+shift+d` (§6.2). Updates from GitHub releases (§9.2). The SSH host is the server's `--vscode-ssh-host` alone, with no per-laptop override (§7.3). Off-LAN use is Tailscale's job, with no relay (§2.1). No standalone CLI for now (§3.3). An **event-kind registry, added now** (§9.1, §10.0), so the contract is checked statically and the context is in the code rather than in memory. One Drydock (§6.4). Mutations only from local and SSH windows (§8). Typed-name approval, revisited only if it proves too much friction (§10.4). | |
| Testing | Unit (Vitest, no `vscode`) · manifest contract · **client tier** (the protocol client against real Drydock behind real Caddy, no VS Code) · **extension-host tier** (`@vscode/test-cli`, headless under xvfb, running the bundle unpacked from the `.vsix`) · a nightly keyboard smoke test on the installed `.vsix` · Spike 05 for the VS Code behaviour this design assumes and has not measured · and rituals for the real Remote-SSH attach, which CI cannot reach. | §12 |

## 1. Problem & goals

The operator spends the working day in VS Code on a laptop, connected over Remote-SSH to the server that runs Drydock. Today, getting from "I want to work on repo X" to an editor attached to X's container means switching to a browser tab: find the repo, press *Clone*, watch the card, and press *Open in VS Code* (design §6, *Opening a workspace in VS Code*), which bounces through the OS URL handler and a confirmation dialog back into VS Code. The extension removes the browser from that loop.

### Functional requirements

- **First two server phases: R, the event-kind registry, and T, API tokens** (§9.1, §4, §10.0, §10.0a). Then **E1, the extension's first iteration:** sign in with a token, see the repositories the GitHub App can reach (`GET /api/repos`), clone one (`POST /api/workspaces`), follow its eight steps to `running`, and connect: open the running container in a new VS Code window at the clone's folder. Also: the running workspaces, *Connect* to any of them, and *Open in Drydock* (the web UI page) for anything the extension does not do itself.
- **From the keyboard alone.** Every action is a command. The hub is one quick pick reachable by one shortcut, and the whole of E1's flow (shortcut, type part of a name, Enter, Enter) needs no mouse.
- **Host-access approval in E1.** A repository that uses docker-in-docker asks for `privileged`, and its first clone stops at `needs_approval` before any container exists (overall §6, step 3). Without approval in the extension, the E1 flow would end with *go to the browser* for exactly the repositories the owner uses most (§10.4).
- **Eventually, most of the web UI.** E2 and E3 add the workspace lifecycle, logs, the Claude identity and login, and ports (§10). Secrets stay web-only (§10.6).
- **Never a second opinion about state.** What the extension shows is what the server's event stream says, through the same reducer the web UI runs (§5). A clone pressed on the phone shows up in the editor, and the reverse.

### Non-goals

- **Not a replacement for the web UI.** The phone and tablet stay first-class clients (overall §13.1). The extension is a second front end, and the web UI stays the complete one.
- **Not a Drydock inside VS Code.** No webview hosting the web UI: the UI sends `frame-ancestors 'none'` (frontend §8), and a webview would bring back a browser with an ambient credential, which §8 below works to avoid.
- **Not a remote-development tool.** Remote-SSH and Dev Containers do the connecting. The extension computes nothing about containers. It hands VS Code the authority Drydock already builds (`internal/vscode`).
- **No Marketplace listing in E1.** §9.2.

## 2. Binding constraints

Six facts shape the design. Three of them overturn the obvious approach.

### 2.1  The laptop must reach Caddy, not just the server's SSH port

A UI-side extension makes its HTTP requests from the laptop. It reaches Drydock exactly as the laptop's browser does: by resolving the UI host and connecting to Caddy on `:443` over the LAN. It does **not** ride the Remote-SSH connection. If the laptop can SSH to the server but not reach Caddy (off the LAN, over a jump host), the extension cannot reach Drydock, and neither can the browser. That is the web UI's reach, inherited on purpose (overall §13.1: LAN, behind Caddy).

**Off the LAN, the owner uses Tailscale, and no relay is designed** (decided). Over the tailnet the laptop reaches Caddy as it does on the LAN. What has to hold:
- **The UI host resolves to the server's tailnet address** when the laptop is on the tailnet: split DNS or a MagicDNS override for the same name. It must be the *same* name, not a tailnet-only alias. The certificate is for that name (overall §13.1), Caddy's strict `Host` matching and Drydock's `HostAllowed` accept only it, and `drydock.url` is one setting (§6.4).
- **`--vscode-ssh-host` resolves from the laptop both on and off the LAN.** It is the only source of the SSH authority (decided, §7.3), so pick a name the laptop's SSH configuration reaches either way, for instance one that resolves through the tailnet everywhere.
- **`X-Forwarded-For` becomes the laptop's tailnet address.** That matters only to the password lockout's per-source key, which the extension never touches (§4.3).

Nothing in Drydock changes for Tailscale. These are deployment facts, recorded so a tailnet-only alias does not quietly break Host matching or the link.

### 2.2  The extension host is Node, and Node is not a browser

The browser gives the web UI four things the extension host lacks. One is not needed at all, and three need a deliberate replacement:

| Browser gives the web UI | Extension host has | Consequence |
|---|---|---|
| A cookie jar that stores `__Host-drydock` and sends it back | `fetch` with no jar | Not needed: the extension authenticates with a bearer token it attaches itself (§4). |
| An `Origin` header on every mutation, set by the browser and unforgeable by page script | No `Origin` unless the code sets one | The extension sets it (§4.3). |
| `EventSource`, with `Last-Event-ID` reconnects | No `EventSource` by default, and the standard one cannot send an `Authorization` header anyway | A small fetch-based SSE reader (§5.3). The server already accepts the resume point as the `Last-Event-ID` header (`internal/api/events_routes.go`). |

### 2.3  A container window runs repository-controlled code next to ours

When the operator connects, the new window has two extension hosts. The local one, on the laptop, runs our extension. The remote one, inside the attached workspace container, runs extensions the repository (or the agent working in it) can put there: it can drop one into the container's own `~/.vscode-server/extensions`, and the attach flow installs the extensions that the container's configuration metadata names. That remote host reaches the window in three ways the web UI never had to consider. A page cannot call into another tab, but these can:

- **Our commands.** `vscode.commands.executeCommand` routes across the window's extension hosts. That is how UI and workspace extensions are meant to talk. So `drydock.*` in that window's local host can be invoked from the container.
- **The workbench's commands**, not only ours. That includes the quick-input accept commands, and possibly commands such as `workbench.action.terminal.newLocal` followed by `…terminal.sendSequence`. If those work from a remote host (unmeasured; Spike 05, §12.9), a container window can run code *on the laptop*, which then reaches whatever the laptop holds, our stored credential and the browser's cookies alike.
- **Settings.** `machine`-scoped and narrower settings can be set in the remote's settings file inside the container (`~/.vscode-server/data/Machine/settings.json`), which Dev Containers fills from `customizations.vscode.settings` in the image's `devcontainer.metadata` label, and Drydock's `up` puts that label on the image. In that window the remote value wins. A Drydock setting a repository could move is a Drydock setting a repository controls. (This path is read from how the two extensions document settings, not yet measured here; Spike 05 #6 measures it.)

§6.4 and §8 are the design's answer, one bullet at a time. **Our commands:** §8's rule 1 refuses every mutation in a container window, which closes this path completely, provided a remote caller's command runs in that window's own local instance (Spike 05 #2). **Settings:** `drydock.url` is `application`-scoped, and the cookie is bound to the origin it was minted by. **The workbench's commands** are not ours to close: **the extension cannot make a container window safer than the attach itself**, and the web UI's existing *Open in VS Code* link already opens exactly such windows. §4.5 weighs it.

### 2.4  *Connect* depends on two proprietary extensions

The link opens `attached-container+<hex>@ssh-remote+<host>`. Resolving it takes Microsoft's **Remote - SSH** (`ms-vscode-remote.remote-ssh`) for the parent authority and **Dev Containers** (`ms-vscode-remote.remote-containers`) for the attached container. Both are closed source, licensed only for Microsoft's VS Code builds, and unavailable on Open VSX. Everything else in the extension (catalog, clone, lifecycle) works without them. §7.3 explains why that argues against declaring them as dependencies.

### 2.5  The UI no longer ships with the server

Today `internal/web/dist` is embedded in the binary, so the UI and the API always match. A `.vsix` the operator installed last month can be older or newer than the server it talks to. Nothing in the API says which version it is. The only version anywhere is `drydock version` on the host (`cmd/drydock/main.go`, stamped by `-ldflags -X main.version=`). §9 adds the handshake.

### 2.6  The link is in views, not in events

A workspace's `vscode` field (`{configured, url}`) is filled from Docker as `GET /api/workspaces` and `GET /api/workspaces/{id}` are served (`internal/vscode` `Linker.Fill`), never stored, and **carried by no event**. A rebuild renames the container. So, like the web UI (`web/src/stores/reducer.ts` `becameRunning`, `vscodeLink`), the extension refetches the view when a `workspace.state` moves to `running`, and trusts only a link read from a view no older than both the latest state change and the latest container change (`vscodeAt >= stateAt && vscodeAt >= containerAt`). Following progress on the stream is not enough to connect.

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
| A `drydock` CLI client that the extension shells out to | It adds a second binary on the laptop to version and ship, and a credential on disk outside `SecretStorage`. **Decided: no standalone CLI for now**, and none as the extension's engine. One may come later. It would share the client tier's protocol tests. |
| The Unix socket, from a workspace-side extension | §3.1 and §3.2. |

## 4. Authentication: scoped API tokens, built first

**Decided (owner):** the extension authenticates with a **scoped, revocable API token** minted in the web UI. Tokens are a server phase of their own, **T, before E1**, so nothing is built twice: no cookie path in the extension to retire later. The extension never holds a session cookie and never sees the operator's password.

### 4.1  The token

- **Format:** `drydock_` followed by 43 base64url characters (32 random bytes). The prefix makes a token recognisable to secret scanners, to the canary sweep and to an operator who pastes one in the wrong place, as GitHub's `ghs_` does.
- **Storage:** a new table, `api_token`, holding the token's SHA-256 (as `auth_session` holds the cookie's), a label, its scopes, `created_at`, `last_used_at`, `expires_at` and `revoked_at`. Never the token. CLAUDE.md's *Store no credential but the App private key* gains the token hash beside the session hash, a hash of the same kind.
- **Lifetime:** an `expires_at` chosen at mint: 90 days by default, at most a year, and no idle expiry (a laptop that sits unused for a fortnight should not lose its token). Revocation is immediate.
- **Scopes:** `read` (every `GET` that is not `session`-scoped, and the stream), `lifecycle` (clone, start, stop, rebuild, delete, restart session, catalog refresh, *Check now*, the Claude login, ports) and **`approve`** (`workspaces.approve` *and* `workspaces.decline`). `approve` is a separate checkbox, **default off**, with the web card's sentence about what approving means beside it. **Never grantable to a token** (scope `session`): `secrets.*` (reads included), the token routes themselves, the session routes (the device list among them), *Sign out everywhere*, and `GET /preview/authorize`. That last one mints a preview token bound to the caller's *session* (`internal/api/preview.go`, `SessionFrom(…).ID`) and is a browser navigation, so no bearer has business there. Every `AuthRedirect` route refuses a bearer outright, before scope. And one scope every credential holds, **`self`**: `DELETE /api/auth/token` revokes the calling token. A cookie request to it is `403 forbidden_scope`: a cookie session is not a token, and signs out through its own route.

**The stream is scoped too, not just the routes.** A token that may not `GET /api/secrets` must not learn the same names from the event stream either. `secret.created`, `secret.rotated`, `secret.updated` and `secret.grants` carry a secret's name, reach, description and grants (`internal/secrets/store.go`), and `api_token.*` names API tokens. So every event kind has a scope in the registry (§9.1), and **a kind missing from `Scopes` is denied to every bearer credential** (the filter fails closed; `TestKindRegistry` keeps the table complete, so this is the belt). The broker's GitHub-token events, `token.issued` and `token.refused`, are `read`: they carry `{scope, expires_at}` and `{scope, reason, permissions}` with fixed messages, never a token value or a secret name. **A bearer credential's stream, and the `events` of any view it reads, carry only kinds within its scopes.** That covers live events, the `Last-Event-ID` replay and the detail view's recent events alike, so no path, replayed or not, shows a `session`-kind event to a token. The filtered events still advance the stream position, so a reconnect does not resync for them: each is sent as a bare `id:` line. **That leaks a little, deliberately:** a token's stream learns *that* and *when* a session-only event happened (an id it was not shown), never *what* — not its kind, data or message. Hiding even that would need the stream to renumber, which breaks `Last-Event-ID` for every client. A cookie stream is unchanged and receives everything.

### 4.2  Minting, pasting, revoking

**Minting is the web UI's.** Settings gains a *Tokens* section. *Create token* asks for a label (pre-filled *VS Code on <device>*), the scopes (`read` and `lifecycle` ticked, `approve` not), and the lifetime. It then shows the token **once**, with *Copy*. The routes, all authenticated by the **cookie session only**:

| Route | Name | Does |
|---|---|---|
| `GET /api/tokens` | `tokens.list` | Every token's metadata: label, scopes, created, last used, expires, revoked. Never the token, and never its hash. |
| `POST /api/tokens` | `tokens.create` | Mints one. `201` with `{id, token, …}`: **the one response in the API that carries a credential**, which is exactly what minting is. `Cache-Control: no-store` (as every JSON answer already says), never written to an event or a log. `api_token.created` carries the metadata alone. The web UI shows the body on its own screen and never hands it to the reducer, as the existing `sendForResult` exception does for an operation's result (`web/src/api/client.ts`). |
| `DELETE /api/tokens/{id}` | `tokens.revoke` | Sets `revoked_at`. `api_token.revoked`. |
| `DELETE /api/auth/token` | `auth.token.delete` | **A token revoking itself**, authenticated by the bearer token, scope `self` (which every token holds, and a cookie session does not). It is what the extension's *Sign Out* calls, so signing out ends the credential rather than leaving it live and forgotten. |

**Pasting is the extension's.** *Drydock: Sign In* explains in one line where tokens come from, and offers *Create a token in Drydock*, which opens `<drydock.url>/settings#tokens`. It then opens `showInputBox({ password: true, ignoreFocusOut: true })` for the paste. The extension checks the shape (`drydock_` + 43 characters), calls `GET /api/meta` with it, and only on a `200` stores **`{origin, token}`** in `context.secrets`: the token and the exact origin it was used against. It attaches the token only to requests for that origin. A `drydock.url` that no longer equals it means *Sign In* again, never the old token sent somewhere new.

**What the extension knows about its token.** `GET /api/meta` (§9.1) answers, beside the version, `credential: {kind: "token", label, scopes, expires_at}`. So the extension knows without trial and error whether it may approve. Without `approve`, the review document's action is *Review in Drydock*. It also knows when the token expires: a status-bar warning appears seven days before, with *Create a new token*.

**Revocation and expiry, as the extension sees them.** A revoked or expired token is a `401` on its next request, and **only** a bad token is: when Drydock cannot read its token store (a busy or broken database) the answer is `503 unavailable` with `Retry-After`, and the bookkeeping write of `last_used_at` is best-effort, so its failure never refuses a request. The 401 is authoritative revocation, and nothing else may spend it. The stream's per-heartbeat liveness check (`EventRoutes.Alive`, today `Sessions.Alive` keyed by session id) is generalised to the credential, so a revoked token's stream ends within one heartbeat, and the reconnect gets the `401`. The extension's one 401 path clears the stored entry and shows *Your Drydock token was revoked or has expired*, with *Sign In*. A `403 forbidden_scope` is not a 401. It leaves the token in place and replaces that one action with *Review in Drydock*.

### 4.3  How a bearer token passes the gate (verified against the code)

Today `SessionGate.Authenticate` (`internal/api/gate.go`) reads the `__Host-drydock` cookie, looks it up through `auth.Sessions.Lookup`, and attaches the session to the request's context. `wrap` (`internal/api/mux.go`) runs Host, then auth, then `Origin` for mutating routes, then the handler or `501`, and `api.Unrouted` gates an undeclared path in the same order. Phase T changes it like this:

1. **`Authenticate` accepts one credential, and only one.** A cookie, as today, or `Authorization: Bearer drydock_…`, looked up by its SHA-256 in `api_token`, unrevoked and unexpired. **A request carrying a cookie together with any `Authorization` header is refused (`401`)**, whatever scheme the header names, since an ambiguous request is not one to give the benefit of the doubt (the same reasoning as `OriginAllowed` refusing two `Origin` headers). The attached value becomes a credential: kind, id, and scopes (a cookie session holds every scope; it is the operator). Any bad credential is the same `401`, never saying which; a store Drydock cannot read, for a cookie as for a token, is `503 unavailable` instead, since it says nothing about the credential.
2. **A new step, scope, runs after auth and before `Origin`:** `Route` gains a `Scope` field (`read`, `lifecycle`, `approve`, `self`, or `session` for routes no token can reach). A credential without the route's scope gets `403 forbidden_scope`. **Protected by default:** a route whose `Scope` is unset is refused to every token, as if it were `session`, and the route-table meta-test fails on any route without an explicit scope. A route added later is then closed to tokens until someone decides otherwise, the scope counterpart of *auth is middleware around the whole mux*. Mutation-checked by clearing one route's scope. An `AuthRedirect` route (`/preview/authorize`) refuses any bearer at the auth step, whatever its scope says. Auth still precedes the `501`, so the route-oracle property holds. Scope comes after auth, so only an authenticated caller learns a route's scope.
3. **`Origin` is unchanged, and still required of a token.** A bearer token is not ambient: no browser attaches it on a third party's behalf, so CSRF, which the check defends, does not reach it. The check could be skipped for bearer requests. It is kept anyway: the extension sends the UI origin (§4.4), and keeping it means the gate has no credential-dependent branch, and the existing `Origin` meta-tests stay exactly as they are.
4. `last_used_at` is written at most once a minute per token, so a busy stream does not become a write per request.

**What it does to the invariants:**

| Invariant (CLAUDE.md, overall §13.5) | With tokens |
|---|---|
| *Auth is middleware around the whole mux* | Unchanged in shape: one `Authenticate`, two credential sources, both checked before any handler. The route-table meta-tests apply as they are. A new one walks every route with a token lacking that route's scope (→ `403`, handler not run) and with it (→ handler runs). Every `session`-scoped route refuses every token, every route has an explicit scope (an unset one is a failure by name), and every `AuthRedirect` route refuses a bearer. |
| *Only the sign-in POST is unauthenticated* | Unchanged. Minting is cookie-authenticated and mutating, and the meta-test's count of `AuthNone` routes stays 1. |
| Sign-in lockout and rate limit | The password route is untouched, and the extension never calls it. A bearer lookup is not a sign-in attempt and never counts toward the lockout. That way a stale token in a busy extension can never lock the operator out of the web UI. Guessing a token is 256 bits of work. The lookup costs what the cookie lookup on every request already costs, and that is not rate-limited either. |
| *Exact-match `Origin`, fail closed* | Unchanged, for every credential (step 3). |
| *Store no credential but the App key* | A token hash beside the session hash; the wording is extended. |
| Redact by default | A token never reaches an event, the log or Caddy (the UI site has no access log). The canary sweep seeds a token. The mint response is the one place it travels, by design. |

### 4.4  The `Origin` the extension sends, and why it is not forgery

The extension sends `Origin: https://drydock.example.com`, byte for byte the configured UI origin, on every mutating request.

**This is acceptable because of what the check defends, and against whom.** Overall §13.3 states the threat: *a page you visit makes your own browser issue the requests*. The browser attaches the cookie because it is ambient, and the `Origin` header is the one thing the attacking page cannot choose. The check is therefore a statement about *which page induced a credentialed browser request*. It was never client authentication: `curl` with a credential has always been able to send any `Origin` it likes. A client that holds its own non-ambient token gains nothing from the header that it could not already do.

**The reasoning holds only while the extension has no ambient credential.** The token lives in `SecretStorage` and is attached by our code to requests our code decides to make. Nothing in VS Code attaches it automatically: no webview, no `http.proxy` agent, and no URL a third party can make the extension fetch. A design that let content (a webview, a link in a markdown preview, a command argument from another extension) cause an authenticated request would rebuild the browser's confused deputy inside the editor. §8's rules exist to prevent that, and §12.8 tests them.

### 4.5  What was considered and decided against

- **The password sign-in with the cookie in `SecretStorage`** (an earlier draft's E1). No server change, but the extension would have held a full operator session that a client choosing not to touch secrets still *could* touch, and it would have been built and then retired. The owner chose tokens first.
- **Step-up authentication for approvals** (re-enter a password to approve). Overall §13.5 refuses re-authentication on destructive routes because it trains habituation, not care, and an approval is a reading problem, not an identity problem. The `approve` scope, granted deliberately, and the typed confirmation (§10.4) do that work.

**The residual risk, plainly.** Attaching to a container whose repository you do not trust gives that repository's code the workbench of that window, and possibly the laptop (§2.3, Spike 05 #5). If it reaches the laptop, it reaches the browser's cookie as well as the extension's token. A token's scope caps only what the extension's own credential adds: no secrets, no other sessions signed out, no tokens minted, and approvals only if the operator granted `approve`. The extension neither causes nor fixes the attach's risk, and must not claim to.

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

**As built (the core PR).** `web/src/core/` holds `types.ts`, `reducer.ts` (with `reducer.fixtures.ts` and the reducer specs), `resources.ts`, `settle.ts`, `marks.ts`, `errors.ts` (`ApiError`), `messages.ts`, and `lib/` (`workspaceCard`, `capacity`, `identity`, `portDiagnosis`, `time`, plus the new `approval` and `text`, §10.4). Where it differs from the plan above:

- **The files moved, and their importers were rewritten**; only the settling predicates are re-exported from their old stores (`stores/workspaces.ts`, `stores/identity.ts`), and `ApiError` from `api/client.ts`. `stores/ports.ts`'s port predicates, `lib/resources.ts`, `lib/portCount.ts` and `lib/login.ts` stay in web until a phase of the extension needs them.
- **`Marks` does not own the in-flight record.** Its owner does, through a `MarkState` (`get`/`set`), so the web UI keeps `inFlight` as Pinia state that `$reset` clears; `ownState()` is the self-held form the extension uses. The clock is a `Clock` of `now()` and `after(ms, fn) → cancel`. `settlesJobOr` is the job clause `mutate` adds, so the extension composes it the same way.
- **The compiler half of the boundary needs `noResolve`.** `lib`/`types` alone leave `import 'vue'` resolving from `web/node_modules`; `noResolve` keeps the program to core's own files, so a package or a path outside core is `TS2307`. Specs are excluded from both rules: they read Go source with `node:fs`, and some drive the mock backend. A `/// <reference lib="dom" />` passes the compiler's rule, so the boundary spec also refuses every triple-slash directive in a core module.

**What it buys.** The extension does not get "a similar reducer". It gets *the* reducer. Ordering by event id, idempotent replay, "only the workspace list drops a workspace", the snapshot-tag rule for marks in a `resync` gap and the fleet identity override are fixed once, for both clients, and tested once.

### 5.3  The stream in Node

`core` has no stream. The extension's `sse.ts` is about 150 lines over `fetch`:

- `GET /api/events` with `Authorization: Bearer …`, `Accept: text/event-stream`, and `Last-Event-ID` on a reconnect. The server honours the header, and the `?last_event_id=` query the web UI needs only because a new `EventSource` cannot send a header.
- It parses frames: `id:`, `data:`, comments (`: ping`, the 20 s heartbeat), and the one named event `resync` (`events_routes.go`'s wire-format comment). Unknown kinds go to the reducer's unknown-kind branch.
- **It classifies failures**, which `EventSource` could not (frontend §2.3). A `401` takes the one 401 path. A network error or a quiet stream (no ping for 45 s) gets backoff with the web UI's schedule (`BACKOFF_MS`). Every reopen runs the refetchers, as `stream.ts` does.
- Caddy compresses the stream (`encode`). `fetch` decodes it, and the browser tier already proves the frames are not buffered (testing §3.2's SSE row).

### 5.4  Marks, and the one mark that does not end on `workspace.job`

Every mutation the extension sends follows frontend §4.2 to the letter: begin a mark, send, discard the `202` body, and end the mark on the settling event or on a snapshot showing the action over. **Correction to the earlier sketch:** not every mark ends on `workspace.job`. A job action (start, rebuild, stop, delete, approve, supervisor) does, through `settlesJob`. **A clone does not.** The client has no workspace id until the `202` body names one, and reading that body is what §4.2 forbids. So a clone settles on the create's first `workspace.state` carrying its `repository_id` (`settlesClone`), which is *the row exists*, not *it is running*. Decline has no job and settles on the request disappearing.

So E1's *clone and follow* is two things. The mark ends quickly, and following to `running` is plain entity state: the workspace's `state` and step timeline from the reducer, shown in a `withProgress` notification until `running` (then *Connect*), `failed` (the failed step's sentence and *Open in Drydock*), or `stopped` with an `approval` (*krelinga/alpha needs host-access approval* and **Review**, which opens the review document, §10.4). After an approval the follow continues: the approve job resumes the run, and the same notification carries it to `running` and *Connect*. The workspace id is learned from that same `workspace.state` event, never from the `202`. A clone of the same repository from another device settles the press too, and is followed the same way.

### 5.5  One stream per window, held while something needs it

Each VS Code window has its own local extension host, and so its own stream and reducer. `SecretStorage` is shared, so one sign-in serves every window. `context.secrets.onDidChange` spreads a sign-out, and the server's own revocation ends every stream.

**The stream is held while any of these is true, and closed five minutes after none is:**

- a Drydock view is visible;
- a mark is in flight;
- **a follow is active**: a workspace this window cloned, started, rebuilt or approved, until it reaches `running`, `failed` or a stopped-with-approval state. A clone's *mark* ends when the row exists (`settlesClone`), but an image build can take many minutes after that, and the progress notification and the *Connect* offer depend on the stream for all of it. A follow survives a reconnect, since the stream's own `Last-Event-ID` replay or `resync` closes the gap, and it ends only on one of those states, on `workspace.gone`, or when the operator dismisses the notification.

Activation is `onView:drydock.*` plus the implicit `onCommand`, so a window that never touches Drydock never makes a request. Ten idle windows hold no streams.

**The cost, accepted for E1:** the status bar's fleet identity warning (§6.3, frontend §6.6) is only as current as the stream. It appears once the operator touches Drydock in a window, and goes quiet five minutes after they stop. So the extension is not a monitor. The web UI and the Claude app's own failure are where a `blanked` login is noticed. E2 adds an opt-in, `drydock.watch` (application scope, default off): with it on, activation also happens at `onStartupFinished`, and **the focused window alone** holds the stream (handing it over on `window.onDidChangeWindowState`), so the warning stays live for the cost of one connection, not one per window. With the setting off, an `onStartupFinished` activation reads the setting and does nothing else: no `SecretStorage` read, no timer, no listener but the configuration-change one, and no request. The extension-host tier asserts that with a recording `Secrets` and the tap, and that turning the setting on starts exactly one stream, in the focused window.

## 6. Commands, quick picks, keybindings, views

### 6.1  Commands

Every action is a command with id `drydock.<verb>`, category *Drydock*, listed in the palette. Commands take **no arguments that can cause a mutation** (§8): tree items and menus pass a workspace id only to commands that read or connect, and a mutation asks its question again.

| Command | Phase | Does |
|---|---|---|
| `drydock.signIn` / `drydock.signOut` | E1 | Paste a token minted in Settings (§4.2). Sign out is `DELETE /api/auth/token` (the token revokes itself), then the secret is cleared. |
| `drydock.go` — *Go to Workspace…* | E1 | **The hub.** One quick pick: running workspaces first (Enter connects), then other workspaces (Enter opens their actions), then repositories without a workspace (Enter clones). Same ordering and actions as the web UI's home, computed by `lib/workspaceCard.ts`'s `rowAction`/`cardStatus` from core. |
| `drydock.clone` — *Clone Repository…* | E1 | Pick a repository → `POST /api/workspaces` → follow (§5.4). At the cap, the item says *At the cap (N): stop a workspace first*, and Enter opens *Stop…* (E2) or, in E1, the web UI. Never a disabled item. |
| `drydock.connect` — *Connect to Workspace…* | E1 | Pick a running workspace → §7. |
| `drydock.reviewApproval` — *Review Host Access…* | E1 | Pick a workspace with a pending request (or take the tree item's id: opening a read-only document is not a mutation) → the review document (§10.4). |
| `drydock.approve` — *Approve Host Access…* | E1 | Only on the active review document's snapshot: the typed full name → `POST …/config-approval` with that snapshot's hash. No keybinding, never in the hub. |
| `drydock.decline` — *Cancel Host-Access Request* | E1 | On the active review document: modal → `DELETE …/config-approval`. |
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
- **macOS and Windows defaults, and OS-level bindings**, cannot be read on a Linux runner. They are a step in the keymap ritual (§12.11), run when the binding or the pinned VS Code version changes.
- **No `when` clause on the hub binding.** It must work in every window, including a container window, where the hub still reads and connects (§8). Tree-scoped bindings use `focusedView == drydock.workspaces`.

**The keyboard-only connect.** `ctrl+alt+shift+d` → type `alp` → the running `krelinga/alpha` is first → Enter → a new window opens attached. For a repository with no workspace, Enter clones it and the progress notification follows. When it reaches `running`, two things happen and neither takes focus:

- the notification turns into *krelinga/alpha is running* with **Connect** as its primary action. VS Code's own `notification.acceptPrimaryAction` (`ctrl+shift+alt+a` on Linux and Windows by default, unverified per platform; checked in the keymap ritual) presses it from the keyboard;
- the workspace moves to the **top of the hub**, marked *ready*, so the shortcut and Enter connect.

An earlier draft popped a *Connect now* quick pick. That was wrong: it arrives minutes later, while the operator is typing in an editor, takes the keystrokes and connects on the next Enter. Nothing the extension shows unprompted takes focus. `drydock.connectWhenReady: true` (application scope) connects without asking, for operators who want exactly that.

### 6.3  Views and the status bar (secondary)

- **An activity-bar container, *Drydock*,** with two tree views, *Workspaces* and *Repositories*. Items carry `contextValue` (`workspace.running`, `workspace.stopped`, `repo.clonable`, …) for `view/item/context` menus, and a default command on Enter (connect, or open actions). A `viewsWelcome` shows *Sign In* when signed out and *Connect Drydock* (set `drydock.url`) before configuration.
- **A status bar item**: `Drydock: 2 running`, with the stream's phase: the web UI's quiet marker after 5 s disconnected, and *idle* once the stream has closed (§5.5), so a stale count never looks live. **The fleet identity override (frontend §6.6) lives here**: a `blanked` login is one status-bar warning, *Signed out. Sign in again.*, with one action. Ten tree items each offering *Restart session server* would be the mistake §6.6 exists to prevent. `absent` gets its own sentence, *No one has signed in yet.*
- **Never a disabled item.** An action that cannot work is replaced by the one that can (frontend §6.6): a `needs_approval` workspace shows *Review host access…* (§10.4), or *Review in Drydock* when the extension's token lacks `approve`, and a server outside the `api` range shows *Install the matching extension* (§9.1).

### 6.4  Settings

Both settings are **`"scope": "application"`, with `"ignoreSync": true`**. Application scope means user settings on the laptop and nowhere else: VS Code ignores the key in workspace, folder and remote settings, so a repository cannot move it through `.vscode/settings.json` or a container's `customizations.vscode.settings` (§2.3). `ignoreSync` keeps a URL that is right for one laptop from being synced to another. Earlier drafts had `machine` scope, which would have let a container window redirect the credential, and would also have left an SSH window looking unconfigured, since a `machine` value in user settings does not apply in a remote window.

- `drydock.url`: the UI origin. It must be `https://`, a lowercase host and an optional port, with nothing after it but an optional `/`; it is read by pattern, not by `new URL` (which would fold the capital this must refuse), and written as `URL.origin` would write it (`:443` dropped). Anything else is refused with a sentence saying why. The server refuses a capitalised origin at startup (overall §13.5), so a capital letter here would make every mutation `forbidden_origin`. A change to it does not carry the token across (§4.2): a token whose origin the setting no longer names is sent nowhere, and nothing is revoked by the change itself, since the Settings editor writes on every keystroke and a switch may be undone. Once the value has held still for three seconds and is a valid origin other than the token's, the focused local or SSH window asks once per pair: *Sign out of <old>* (revoke there, best effort, and forget) or *Keep*. An empty or invalid value asks nothing. A new sign-in, by contrast, revokes the token it replaces without asking. **One Drydock** (decided): there is one `drydock.url`, one stored token and one stream per window.
- `drydock.connectWhenReady`, and from E2 `drydock.watch` (§5.5).

The manifest test asserts both scopes and `ignoreSync` (§12.3). Spike 05 checks that VS Code honours them against remote settings, and whether remote settings can move the `http.*` settings that govern the *local* extension host's `fetch` (`http.proxy`, `http.proxyStrictSSL`, `http.systemCertificates`). Those are VS Code's, not ours, so if they can be moved, the client builds its own TLS agent and ignores them (§12.8).

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

*As built (after #160's review).* `vscodeURL` (core, so the web UI's link gets it too) decodes both hex payloads and holds them to exactly what `internal/vscode` writes: the container's must be the bytes of `{"containerName":"/<docker name>"}` and nothing else — the Dev Containers extension also reads a `settings.host` there, which would open a connection of the payload's choosing — and an SSH host's `{hostName, user?, port?}` with `config`'s patterns. **What neither client can check is which SSH host the server chose.** A compromised Drydock could put any host `config` would accept in the link, and VS Code would connect there with the operator's own SSH configuration. So the host is shown, never only implied: the hub's item for a running workspace says *over SSH to <host>* (`vscodeSSHHost`), the notification at the end of a follow says *Connect opens it over SSH to <host>*, and Show Log records it for every connect. The operator's defence is the same as for the web UI's link, which names the same host: an SSH host they do not recognise is not one to connect to.

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

Instead, *Connect* checks `vscode.extensions.getExtension('ms-vscode-remote.remote-ssh')` and `…remote-containers` first. If either is missing, Connect is replaced by *Install Remote - SSH and Dev Containers*, which runs `workbench.extensions.installExtension` for both and continues. The README states the requirement. And the SSH host in the link is the server's `--vscode-ssh-host`, which must match an entry the laptop's SSH configuration resolves. **Decided: there is no per-laptop override.** The server's `--vscode-ssh-host` is the only source of the SSH authority, which keeps its encoding in one place, `internal/vscode`.

## 8. The container window: commands are an attack surface

§2.3's finding sets these rules. They apply from E1, because connect is what puts the operator in a container window in the first place.

### 8.1  The rules

1. **Primary control: outside an allowlist of window kinds, the extension does not mutate.** Each window's local extension host runs its own instance of our extension, and a command invoked from a window's remote host is served by *that window's* local instance. So the instance can decide from its own window what it may do. `mayMutate(vscode.env.remoteName)` is true only for `undefined` (a local window) and `ssh-remote` (whose remote host runs as the operator's own SSH user, who already has Docker and so the host). For anything else (`attached-container`, `dev-container`, `tunnel`, `codespaces`, a name VS Code adds later), every mutating command, **host-access approval above all**, is replaced by *Do this from a local or SSH window, or in Drydock*, with *Open in Drydock*. It is an allowlist, so an unknown kind of window fails closed. This closes the path from container code to our mutations completely, whatever the caller passes and whatever it can accept. Reading, the hub and connect still work there, since moving to another workspace is the common case.
2. **Depth: a mutating command takes no meaningful arguments.** `drydock.stop(id)` called with an id ignores it and shows its own pick, so the operator, not the caller, chooses the target. Read and connect commands may take an id from a tree item. *As built:* `drydock.connect` and `drydock.openInBrowser` take a workspace id (a ULID, as a string or a tree node's `key`), and **any caller can aim them at any workspace id, with no picker** — the id is not an unforgeable handle, since VS Code passes a tree item's argument as plain data any other extension could send too. That is accepted because neither command changes anything: `connect` opens a new window on the server's own link for that workspace (as the operator could by picking it in the hub, and only from links that pass `vscodeURL`), and `openInBrowser` opens that workspace's page in the browser, where the browser's own session decides what it may do. A caller learns nothing back (rule 3). An unforgeable handle (a per-render nonce map) was considered and not built: it would protect only these two non-mutating commands, and the attach it guards is the operator's own risk already (§8.2).
3. **Depth: every command returns `undefined`.** No caller can read back the workspace list, the repositories or a link.
4. **Depth: E2's lifecycle mutations confirm in a modal.** Start, stop, rebuild and restart session show `showWarningMessage(…, { modal: true })`. Delete requires the typed full name. **Approve is confirmed by typing, not a modal** (§10.4). **E1's clone, sign out, refresh and *Check now* are not modal-confirmed**: they rely on rule 1 alone. Clone takes a slot and runs that repository's build, which is never more than any clone does: a host-access setting still stops at the approval gate (overall §6). The others are cheap to undo. Quick picks are not a confirmation by themselves, since the workbench's quick-input accept commands can be called by other extensions.
   **What the modal claim rests on:** that no command accepts a VS Code modal. That is believed, not measured. A native dialog has no workbench command, but `window.dialogStyle: "custom"` renders it in the DOM, and whether anything can press it there is unknown. Spike 05 measures it under both dialog styles (§12.9). Until then rule 4 is depth on top of rule 1, not a control in its own right.
5. **No ambient credential anywhere.** No webview ever holds the token or makes Drydock requests. All HTTP comes from one client module in the extension host, which sends the token only to the origin it was stored for (§4.2), validates the path against the route patterns it knows, and never follows a redirect to another origin with it.

The extension-installing command of §9.2's update path is a mutation of the laptop, so it is under rule 1 too.

### 8.2  What these rules do not do

They keep our commands and our credential out of a container's reach. They do not make the container window safe. Container code that can run workbench commands may reach a local terminal (Spike 05), and from the laptop it can reach anything. The operator's defence there is the same as for any attach: attach only to repositories you would open on the laptop. The web UI's *Open in VS Code* has carried that condition since it shipped, and the extension makes it neither better nor worse.

## 9. Version skew and release

### 9.1  The handshake: `GET /api/meta`

A new route, **`meta.read`**: `GET /api/meta`, `AuthRequired`, not mutating.

```json
{ "version": "v0.12.0", "api": 1,
  "credential": { "kind": "token", "label": "VS Code on my laptop", "scopes": ["read", "lifecycle", "approve"], "expires_at": "2027-01-11T00:00:00Z" } }
```

- `version` is `main.version`, passed into the server config (`dev` for local builds). The extension uses it only after checking it against `^v\d+\.\d+\.\d+$`. Anything else (`dev`, garbage) is shown as text and never becomes part of a download URL (§9.2).
- `api` is an integer the server bumps when a route, event kind or view field **the extension uses** changes incompatibly. Additive changes (a new route, a new event kind, a new field) do not bump it: the reducer's unknown-kind branch and ignored unknown fields are what keep the old client working.
- **Authenticated, on purpose.** The version is information about the server for someone who can use it, and an unauthenticated route would be a second `AuthNone` on the API mux, which the meta-test forbids. The extension asks right after sign-in (it is how a pasted token is verified, §4.2), and again on every stream reopen, since the server may have been upgraded underneath it. The answer also carries the caller's `credential` (kind, label, scopes, expiry), so the extension knows what its token may do.
- **An older server** has no token support at all, so a token gets `401` from it. Phase T and `meta` ship together, so a server that accepts the token always answers `meta`. The extension reads a `401` on a freshly pasted token from a server it has never reached as *This Drydock does not accept tokens yet (it is older than this extension). Upgrade Drydock.*

The extension declares `drydock.apiRange: [min, max]` in its own `package.json`. The decision is a pure function, unit-tested over the whole table:

| Server `api` | Extension does |
|---|---|
| within `[min, max]` | Everything. If `version` differs from the extension's own, a quiet one-time notice offers the matching `.vsix`. |
| `< min` | Reads only: the lists, follow, and connect, which use routes that have not changed since Phase 2. Mutations are replaced by *This Drydock is older than this extension supports. Upgrade Drydock, or install the extension from its release.* |
| `> max` | The same, with *This extension is older than the Drydock it is talking to* and *Install the matching extension* as the replacement action. |

**The contract is pinned by tests, not by memory**, statically, in two halves.

- **Routes.** `extension/contract.json` lists every route name the extension calls, with method and pattern. A Go test (`internal/api`, `TestExtensionContract`) fails when one is missing from `api.Table` or differs. The route table is data (testing §5.1), so this is exact. The failure message says *bump `api` in `internal/api/meta.go` or keep the route*. Mutation-checked by renaming `workspaces.create`'s pattern.
- **Event kinds, against a registry** (owner's decision: add it now, so the context lives in the code rather than in anyone's memory). Today there is no one place that lists the kinds the server emits. Twenty-five were `Kind*` string constants spread over seven packages (`internal/workspace`, `internal/catalog`, `internal/preview`, `internal/secrets`, `internal/identity`, `internal/login`, …), and twelve were bare literals at call sites (`"token.refused"` in `internal/broker/broker.go`, others in `internal/reconcile`, `internal/secrets` and `internal/server`): 37 kinds in all. Phase R (§10.0) makes one:
  - **`internal/events/kinds.go`** declares `type Kind string` and every kind the server emits as a constant of that type, each with a one-line comment naming its data shape and **its scope**: `read`, or `session` for kinds a token must not see (`secret.*`, and phase T's `api_token.*`; not the broker's `token.*`, which name a GitHub token's scope and never the token), in a table `Scopes map[Kind]Scope` beside the constants. The stream filter (§4.1) reads it. That is the table at the top of `web/src/stores/reducer.ts`, moved to where the kinds are made. The packages' existing `Kind*` constants are removed and their uses name the registry's constants directly (`events.KindWorkspaceState`): a `Kind` constant declared anywhere but `kinds.go`, even one defined as a registry constant, fails the meta-test below. Every literal is replaced. `NewEvent` and `Emit` take a `Kind`, and **`Event.Kind` itself is typed `events.Kind`**, so a hand-built `Event` passed to `Append`, `Commit` or `CommitThen` is held to the registry too. The one place a `Kind` is made from a string is the log's scan of a stored row, which the meta-test allowlists by file and function. The wire format does not change: a kind is still its string.
  - **A meta-test, `TestKindRegistry`**, typechecks every non-test package with `go/types`. Its rules, precisely:
    - **Every expression of type `events.Kind` that is a constant must trace to an identifier declared in `kinds.go`.** The type checker records each constant expression's value and type. A literal converted to `Kind`, implicitly at a call or explicitly by `Kind("…")`, or a constant declared elsewhere (even one defined as a registry constant plus a suffix) fails by file and line. Typing the parameter alone would not do it, since Go converts an untyped string literal to `Kind` silently. So `Emit(…, "token.refused2", …)` fails by name, and so does a literal passed through a helper.
    - **A conversion to `Kind` from a non-constant fails**, except the one allowlisted scan of a stored row. A kind from a variable cannot be checked.
    - **"Used" means reaching a `NewEvent` or `Emit` argument, or an `Event{Kind: …}` literal**, through parameters, local and package variables, closures and single-result helpers (not through a struct field, a map or a multi-value return: a kind that travels that way is emitted directly or the analysis is extended). A registered kind used nowhere that way fails, so a kind that stops being emitted cannot linger as a false promise.
    - **Every registered kind has a scope** in `Scopes`, and every `Scopes` key is registered.

    Its control: the scan finds the known `events.KindWorkspaceState` use in `internal/workspace`, and the registry is non-empty. Mutation-checked five ways: a literal kind at an `Emit`, a literal in a hand-built `Event` passed to `Append`, an unused constant added to `kinds.go`, a `Kind(someString)` from a variable, and a kind removed from `Scopes`.
  - **What the registry does not cover**, by design: a `Kind` made by a generic conversion (`T(s)` with `T ~string`), or written by reflection (`json.Unmarshal`, `database/sql`'s `Scan` into a `*Kind`), which the test cannot see, so no code does either; an untyped string compared to a kind's text, such as the SQL `IN ('secret.undeliverable', 'secret.deliverable')` and the `kind == "secret.undeliverable"` in `internal/secrets/store.go`'s `lastReportedUndeliverable` (a renamed kind would make that read miss silently, so it is rewritten to use the constants in the R PR, and the test cannot force that elsewhere); and **named frames** from `Log.Broadcast` (`resources`) and the stream's own `resync`, which are not kinds. Their names are the stream code's, and the filter passes them to every `read` credential.
  - **`TestExtensionContract`** checks every kind `contract.json` lists as *read* against the registry's constants, read from `kinds.go`, statically, in the ordinary `go test`. The failure says *bump `api` or keep the kind*.

**The observation check is gone.** The client tier no longer tallies the kinds it saw against the contract: the registry answers "does the server emit this kind?" for every kind, not just the ones a scenario happens to provoke, and there is no `unexercised` list to maintain. The client tier still **records real event payloads** for the unit tier's fake transport and the SSE parser's specs (§12.4), and that is the check on a kind's *shape*, which a name-only registry cannot give.

A cheap follow-on the registry makes possible, not required by the extension: a web spec that reads `kinds.go` (as `lib/capacity.spec.ts` reads `Occupying` from Go) and fails when the reducer's comment table or its handled kinds name a kind the server does not emit.

### 9.2  Release: a `.vsix` on every Drydock release

The extension lives in **`extension/`** (beside `web/` and `feature/`) and is versioned with Drydock: one tag, one version, and *the extension that matches* is unambiguous.

- **Version stamping.** release-please is `release-type: go` with one root package. It does not touch `package.json`. `release-please-config.json` gains `"extra-files": [{"type": "json", "path": "extension/package.json", "jsonpath": "$.version"}]`, so the release PR bumps it. A `test/docs` check fails when `extension/package.json`'s version differs from `.release-please-manifest.json`'s.
- **Assets.** The `assets` job builds the bundle (esbuild) and packages it with `npx @vscode/vsce package --no-dependencies -o dist/drydock-vscode.vsix`, a **fixed name** like the others, so `releases/latest/download/drydock-vscode.vsix` always resolves. `SHA256SUMS` covers it.
- **`verify` changes in the same PR.** It asserts the exact asset list (`SHA256SUMS drydock_linux_amd64.tar.gz install.sh`). That line gains `drydock-vscode.vsix`. It checks the hash against `SHA256SUMS` and that the `.vsix`'s `extension/package.json` says the tag's version (`unzip -p … | jq -r .version`), and runs the manifest contract (§12.3) on that unpacked `package.json`. **It runs no Electron.** `verify` is on a bare runner, and an xvfb flake must not hold back a server release. What ties the published bytes to tested bytes is the `extension` CI job on the release commit, which built and tested a `.vsix` from the same source with the same pinned toolchain. The build is not claimed to be reproducible byte for byte, so `verify` cannot prove more than that, and does not claim to.
- **Installing and updating.** (Rule 1 of §8 applies: no update from a container window.) First install: *Extensions: Install from VSIX…*, or `code --install-extension drydock-vscode.vsix`. VS Code does not auto-update a sideloaded extension, so the handshake's notice does: *Install the extension for Drydock v0.12.0* downloads `releases/download/<version>/drydock-vscode.vsix` and `SHA256SUMS` from GitHub, checks the hash, and runs `workbench.extensions.installExtension` on the file. The hash comes from the same origin, so it guards against corruption, not against a compromised release, and the doc says no more for it than that.

*As built (PR 6).* `npm run package` (`vsce package --no-dependencies --skip-license`, `.vscodeignore` leaving only the bundle, `package.json`, the README and the activity-bar icon) writes `extension/dist/drydock-vscode.vsix`. The release's `assets` job checks `extension/package.json` says the tag, packages it with the lock file's toolchain, and appends its line to `SHA256SUMS` (the installer greps only its tarball's line, so an old installer is unaffected). `verify` expects exactly `SHA256SUMS drydock-vscode.vsix drydock_linux_amd64.tar.gz install.sh` (`LC_ALL=C` sorted), checks every hash, the `.vsix`'s `extension/package.json` version against the tag, and runs `test/manifest.spec.ts` from the tag's `extension/` against that unpacked `package.json` (`DRYDOCK_MANIFEST`); `test/install/live.sh --dir` is unchanged and passes with the extra file served. The update path is `src/update.ts` (the decision: a quiet one-time notice when the server's release differs, *Install the matching extension* in place of a mutation when it is newer; rule 1 holds) and `src/node/update.ts` (the download: only for a `vX.Y.Z`, the `.vsix` checked against that release's `SHA256SUMS` before `workbench.extensions.installExtension` sees it, then *Reload Window*). Each download is bounded (60 s, 64 KiB for the sums, 20 MiB for the `.vsix`) and lives in a directory of its own, deleted once VS Code has the file or has failed to take it. The notice says when the server's release is *older* than the extension (installing it goes back), and a version the operator let be is remembered per laptop (`globalState`), so new windows do not ask again. A resumed draft whose tag predates the packaging (no `extension/`, or no `package` script) is uploaded and verified without the `.vsix`, and a tag whose manifest spec cannot read `DRYDOCK_MANIFEST` skips that check, each saying so in the log.

**Why not the Marketplace, yet.** A listing needs a publisher identity and a publishing token in CI (a new secret, and a new party able to ship code to the operator's editor). It invites users the design does not serve: one operator, one server (overall §1). And it would auto-update the extension *ahead of* the server, which is skew by default. Revisit if anyone but the owner runs Drydock. Open VSX does not help: §2.4's dependencies are not there.

## 10. Phasing

Each phase states when it is done, as overall §14 does.

### 10.0  R: the event-kind registry (server, a small PR first)

The registry of §9.1, on its own and first. It is a refactor with no behaviour change, so the existing suite is its proof (the `go test` and the browser tier see the same wire). It is independent of tokens, so it does not wait on T or hold it up. Doing it first means T's new `api_token.created`/`api_token.revoked` are born in the registry.

**Done when:** `kinds.go` names every kind the server emits, each with a scope; no `Kind`-typed constant is spelled outside it; `Event.Kind` is a `Kind`; no registered kind is unused (`TestKindRegistry`, mutation-checked as §9.1 says); and the suite passes unchanged.

**Size:** small but wide. One new file of 37 constants with their shape comments; a mechanical edit at every emit site in roughly a dozen packages; one meta-test of a few hundred lines (the `go/types` load is the only new machinery, and `TestContextRule`'s and `TestRowRule`'s AST walks are the precedent). An afternoon to a day.

### 10.0a  T: API tokens (server, before E1)

The owner's decision: build the credential the extension will keep, before the extension, rather than ship a cookie path and retire it (§4).
- The `api_token` table, with design §4's schema and the golden schema updated.
- `Authenticate` with a bearer, the `Scope` column and its step in `wrap` and `Unrouted`, `forbidden_scope`, and the scope meta-test.
- The stream and view-event filter by kind scope (§4.1), on live events, replay and the detail view's `events`.
- The four token routes (§4.2), with `api_token.created`/`api_token.revoked` events (registered in `kinds.go`, scoped `session`) carrying metadata only.
- `Alive` generalised to credentials.
- The token as `approved_by`.
- *Sign out everywhere* split into its own `session` route (`DELETE /api/auth/sessions`), so scopes stay keyed on route names.
- `GET /api/meta` with `credential`.
- Settings' *Tokens* section in the web UI: create (with `approve` default off), the once-only display, list, revoke.
- Overall §4, §5 and §13.2 and CLAUDE.md's credential invariant updated in the same change.

**As built (server half).** Where the code settled something this section left open, or differs from it:

- **The id is not the hash.** `api_token.id` is a random `tok_…`, and the SHA-256 is a column of its own (`token_hash`), because Settings lists tokens by id and the hash is shown nowhere. `approved_by` records that id for a token's approval, a session's SHA-256 for a cookie's.
- **Scopes are three columns** (`scope_read`, `scope_lifecycle`, `scope_approve`, each a `CHECK`ed 0/1, at least one set), so the vocabulary is the schema's. `self` is every token's and `session` no token's, so neither is stored. **Every token holds `read`**: a mint without it is `400` (the web form shows it always on), since `GET /api/meta`, how a pasted token is verified, is `read`-scoped (§11). `lifecycle` and `approve` are optional.
- **The mint's body** is `{label, scopes, expires_in_days}` (1–365, 90 when absent; a label of 1–100 characters with no control or format characters), unknown fields refused. Its `201` is the token's metadata plus `token`.
- **Revoking** a token already revoked, or none by that id, is `404`; `DELETE /api/auth/token` answers `204`, as a session's sign-out does.
- **A bearer on an `AuthRedirect` route** gets the sign-in redirect, exactly as no credential does, before scope is asked.
- **An undeclared path** (`api.Unrouted`) declares no scope, so a token is `403 forbidden_scope` there and the operator gets the JSON `404`: a token learns nothing of which paths exist under other methods.
- **`DELETE /api/auth/session?all=true`** is `400`, never read as "this device", now that *Sign out everywhere* is `DELETE /api/auth/sessions`.
- **A filtered event** is sent as an `id:` line alone: per the SSE spec that sets the stream's last event id and dispatches nothing, so the position moves past it.
- **A store failure is `503`, never `401`** (review of #149): a failed token read answers `503 unavailable` with `Retry-After`, and the `last_used_at` write is best-effort. The cookie was aligned after #150's review: a failed session read is the same `503` — on an `AuthRedirect` route too, rather than the sign-in redirect — and a session's idle slide is best-effort like `last_used_at`, so a transient `SQLITE_BUSY` no longer bounces the web UI to `/signin` (`TestSessionStoreFailureIsNot401`).
- **`drydock passwd` revokes every token**, in the same transaction as the new hash and the end of every session: a password changed because someone else may know it must also end what they could mint with it. Not in §4 before.
- `Origin` is checked for a token exactly as for a cookie; the gate has no credential-dependent branch past scope.

Its tests: `internal/api` `scope_test.go` (the scope meta-tests), `bearer_test.go` (the real gate and token routes), `streamscope_test.go` (stream, replay, the detail view, `Alive`); `internal/auth` `token_test.go`; `internal/server` `tokens_test.go` (end to end, the canary-named secret over a cookie and a bearer stream, and the canary sweep seeded with a minted token). The web half is Settings' *API tokens* section (`web/src/components/ApiTokens.vue`): create with `approve` off, the once-only display, the list from the stream, revoke settled by its event.

**Done when:** every route-table meta-test passes with tokens in play, with the new scope tests mutation-checked: a `session` route marked `lifecycle` fails by name, a route with its scope cleared fails by name, and `/preview/authorize` answering a bearer fails by name. **A `read` token's stream and replay carry no `secret.*` or `api_token.*` event**: a test creates, rotates and grants a canary-named secret and mints a token. The control is that a cookie stream, open over the same window, receives each of them, and the bearer stream received the interleaved `workspace.state` events (so it was live). The sweep of the bearer client's recorded stream finds the canary name nowhere. Mutation-checked by dropping the filter on replay only. A token minted in Settings reads `GET /api/meta`. A revoked one gets `401` on its next request and loses its stream within a heartbeat. A token without `approve` gets `403 forbidden_scope` from the approve route. And the canary sweep, seeded with a minted token, finds it nowhere but the mint response's single delivery.

**Size:** comparable to Phase 1's session work. A schema row, a gate step, four routes, one meta-test, one Settings section. It is the largest server change the extension needs, now front-loaded.

### 10.1  E1: sign in, catalog, clone, follow, approve, connect

`signIn` (paste a token), `signOut` (the token revokes itself), `go`, `clone`, `connect`, `reviewApproval`, `approve`, `decline`, `refreshRepositories`, `openInBrowser`, `showLog`. The tree views, status bar and handshake. Requires phase T. The `core/` split lands first, as its own PR, now including `lib/approval.ts` and the format-character escape for the web card.

**As built: the PR sequence.** (1) The `core/` split (#153). (2) The `extension/` skeleton and its pure client: the layers and their boundary, the ports, the HTTP client, the SSE parser and stream, the token and `drydock.url` rules, `mayMutate` and the dispatcher, the handshake, link → URI, `signIn`/`signOut`/`openInBrowser`/`showLog`, the manifest spec, `contract.json` with `TestExtensionContract`, release-please's `extra-files` with its `test/docs` check, and a CI `extension` job (unit and manifest only). (3) The stack, the shared harness and the client tier. (4a) The hub, catalog, clone, follow, connect and the status bar. (4b) The tree views, the extension-host tier and Spike 05. (5) The review document and approval, and the recommended baseline check. (6) The `.vsix` asset. Where (2) settled what this document left open:

- **A third layer, `src/node/`**: Node's `fetch` as the `Transport`, importing no `vscode`, so the client tier runs the production transport with no editor. The pure layer is compiled with `types: []` and `noResolve` (as core is), so `fetch`, timers and `console` are all ports.
- **`drydock.signIn` is mutating** (rule 1): it replaces the credential every window uses, and nothing needs it from a container window.
- **A 401 on a freshly pasted token** says it was revoked or expired, *or* the server is older than tokens: the extension cannot tell them apart, and §9.1's single "older server" reading would mislabel the common case.
- **`TestExtensionContract` checks kinds against `events.Scopes`** (the registry's own table, which `TestKindRegistry` holds complete) and requires scope `read`, so a kind moved to `session` fails too; routes must be `AuthRequired` on the API mux with a scope a token can hold.
- **`extra-files` landed with the skeleton**, not with the `.vsix`: `extension/package.json` carries Drydock's version from its first commit, and `test/docs`'s `TestExtensionVersionIsTheRelease` fails on drift or on a lost `extra-files` entry.

**As built (4a).** The window's live state is `src/live.ts` (core's reducer and marks over the stream, holding it per §5.5: a hold opens it, the snapshots and `GET /api/meta` are read after each open, and it closes `IDLE_MS` after the last hold or mark), the flows are `src/flows.ts`, the hub's items `src/hub.ts`, the status bar `src/status.ts`. Where 4a settled what this document left open:

- **A non-running workspace in the hub opens its page in Drydock.** E2 adds its actions there. One waiting on a host-access request says so, and until the review document (PR 5) a follow that ends at an approval offers *Open in Drydock* in place of *Review*.
- **The hub reads, so its clone holds rule 1 itself**: `clone()` checks `mayMutate` as the dispatcher does for `drydock.clone`.
- **Skew's *Install the matching extension* opens that release's `drydock-vscode.vsix` URL in the browser** (`vsixURL`, release versions only). §9.2's in-editor download, hash check and `installExtension` waits for the asset to exist (PR 6).
- **Mutations are gated on skew** (`skewAllows`): clone and refresh. Reads, follow and connect work outside the range.
- **The status bar has no fleet identity override yet**: that is E2's (§10.2), live only with `drydock.watch`. It shows the running count, *reconnecting* after the web UI's five seconds, *idle* once the stream has closed, a token within seven days of expiry, and a version mismatch.

**Size.** Approval adds to E1:
- the review document provider and its three commands (a few hundred lines);
- `lib/approval.ts` in core, with the web card moved onto it;
- the stack's docker-in-docker fixture repository;
- the approval rows in the tests (§12).

It is the largest single addition to E1 after the test stack, and it is what makes E1 usable for docker-in-docker repositories.

**Done when:** from a fresh VS Code on the laptop, with the keyboard only, the operator signs in, clones a testbed repository, watches it reach `running`, and lands in a new window attached to its container at the clone's folder. The same works for a docker-in-docker repository through *Review*, the document and the typed name. The connect ritual (§12.11) has passed once. The extension-host tier passes in CI.

### 10.2  E2: the lifecycle

`start`, `stop`, `rebuild`, `restartSession`, `delete` (typed name), the two logs, `checkClaudeLogin`, the fleet identity override in the status bar, and *Make room* at the cap. Every mutation follows §8.

**Done when:** each command's in-flight mark ends on its settling event in the extension-host tier, including after a forced `resync`. Rule 1's tests (§12.2, §12.5) show none of them sends from a container window, and the hostile-extension test (§12.8) shows none can be driven by another extension's arguments or accept commands.

### 10.3  E3: the Claude login and ports

The login handshake (frontend §6.2): `POST /api/auth/claude/login`, show the authorize URL (`env.openExternal` to open it), and take the code in `showInputBox({ password: true })`. A login code is a credential, so it is never shown back and never logged, and it is a canary in the sweep. Ports: list, enable, disable, open. Opening a preview goes through the browser's own preview handshake (`/preview/authorize`), which needs the *browser's* session. The extension's cookie never reaches a browser.

No new credential: the operator's token needs `lifecycle` (the login and ports are in it).

### 10.4  Host-access approvals, in E1

**Why E1.** The owner uses docker-in-docker in many projects. Docker-in-docker asks for `privileged`, and a clone of such a repository stops at step 3, `resolve config`, as `stopped` with a pending request, before any `up` (overall §6). Without approval in the extension, E1's clone → connect flow would end with *go to the browser* for exactly the repositories the owner uses most, which is a regression from VS Code's own *Reopen in Container*. The cost is bounded by how often it happens. An approval is recorded **per repository** and a later run is asked again only when its subset goes beyond what was approved (`container.Covered`), so the prompt comes once per docker-in-docker repository and again when its configuration's host access changes, not on every clone. E2's start and rebuild can stop for approval too, and use the same flow.

**Still the most security-sensitive surface Drydock has.** Approving `privileged` gives the repository host root (overall §13.4), and frontend §6.5a spends a section making sure the button is not pressed without reading. The request often arises *because of a change in the repository the operator has open in that same editor*, possibly made by the agent. The design below keeps every property of the web card and adds two the editor needs.

#### What the API does today (verified)

- A run that stops for approval writes the request on the workspace row (`pending_approval`). The view and the `workspace.state` event carry an `ApprovalView`: `{hash, added, changed, removed}` (`internal/workspace/approval.go`). The **hash is of the whole requested subset** (`Settings`, canonical JSON, which the client never receives). `added`/`changed`/`removed` are its difference from the repository's current approval, which is what the operator is shown.
- `POST /api/workspaces/{id}/config-approval` with `{"hash": "sha256:…"}` (`workspaces.approve`). `Store.Approve` refuses any hash but the pending one with `409 approval_stale`, re-checks it inside the transaction (so two approvals, or an approval racing a move, cannot both record), supersedes the repository's previous approval, records `approved_by` (the session id), writes `config.approved`, and the provisioner continues the run as an `approve` job. `DELETE` on the same path declines (`workspaces.decline`).
- The web card (`web/src/components/HostAccessApproval.vue`) renders that view and sends `req.hash`, the hash of the very object it rendered.

#### What the extension shows: the review document

*Review host access…* opens a **read-only virtual document** (`drydock-approval:/<workspace id>`, a `TextDocumentContentProvider`, never a webview, §8 rule 5) beside the editor. It has exactly the web card's content:

- the title, *Needs approval: this configuration asks for host access*, with the repository and branch;
- **the warning**: what approving lets the container do, as the drydock user or as root, and that anything in the container, an agent included, can then use it without changing the configuration again;
- **`privileged` is root on the host: Drydock's keys and every other workspace are in reach**, when `privileged` is new or changed;
- **`initializeCommand` runs on this server, not in the container, as the drydock user, which can read Drydock's keys and is root through Docker**, when it is new or changed (the sentence PR #141 added to the web card);
- *New*, *Changed since the last approval* (was / now) and *No longer asked for*, each setting with its source (*the repository's devcontainer.json* or *a Feature or the image*) and its value as JSON text, never the whole configuration.

**One source for both clients.** The sentences and the grouping move into core as `lib/approval.ts` (`approvalSections(req)`), and both `HostAccessApproval.vue` and the extension's document provider render it. A warning added to one client is then in the other, and the web's existing `views/HostAccessApproval.spec.ts` keeps its sentences. **Values are shown as data, never interpreted.** `JSON.stringify` already escapes newlines and control characters, so a value cannot forge a line of the document. Core also escapes Unicode format characters (bidi overrides, zero-width characters) as `\uXXXX`, so a container-written value cannot visually reorder what the operator reads. The same escape applies to **every repository-sourced string** either client shows in the approval and beside it: the branch name, the repository's full name, a setting's source, and a Feature's id. It applies to the web card too, and lands in the core PR. *As built:* `escapeFormat` (`core/lib/text.ts`) escapes Unicode `Cc`, `Cf`, `Zl` and `Zp`, since `JSON.stringify` leaves C1 controls (U+0085 among them) and U+2028/U+2029 as they are, and each can break a line in some renderer; `approvalSections` returns the groups with field, source and values already escaped, and the root sentences as `roots` for the confirm prompt to repeat. The web card escapes the branch and the repository's full name where it shows them (the card heading, the catalog row, the detail view's title).

**A known gap, shared, not new.** The root warnings key only on field names (`privileged`, `initializeCommand`). A request that is host root by another route says nothing extra: `runArgs` with `--privileged`, a mount of `/var/run/docker.sock` or `/`, `capAdd: SYS_ADMIN`. That is a pre-existing gap in the web card, tracked as #144. Because both clients render `lib/approval.ts`, fixing #144 there fixes both, which is one more reason for the shared module.

#### How it is confirmed: typing the repository's full name

From the review document (an editor-title action, or `drydock.approve` while that document is the active editor), *Approve and continue* opens an input box. Its prompt repeats the root sentences that apply, and it asks for the repository's full name (`krelinga/alpha`), compared exactly as delete's confirm is. The box is **never pre-filled, and the name is never its placeholder**: an answer that is already there, or shown greyed for copying, is a keystroke again. The prompt names the repository in prose only. Enter with anything else does nothing. Escape or focus loss (`ignoreFocusOut: false`) cancels.

**Why typing, not a modal.** A VS Code modal's primary button is the first non-*Cancel* item, so Enter approves: one keystroke from a stray Enter (documented as `isCloseAffordance`'s semantics; Spike 05 #4 measures it). `showWarningMessage` has no way to make *Cancel* the default. Typing the name is the friction overall §13.5 already chose for the other irreversible action. It cannot be satisfied by a keystroke meant for something else, and it names the repository being trusted. To be precise about what it buys: it is an **intent check, not a reading check**. It proves the operator meant to approve *this repository*, not that they read the subset. Reading is what the document and its warnings are for, and nothing in either client can force it. This is stricter than the web card's one click, which is open question 2.

**Never one keystroke, anywhere.** No keybinding, no hub or quick-pick item that approves, and no notification button that approves. The notification's action is **Review**, which only opens the document. *Cancel* (decline) is a plain modal: declining leaves the workspace stopped and is safe.

#### Approve exactly what was shown

The client cannot recompute the hash, since it never sees `Settings`. So the binding is this: **the hash sent is the one that arrived in the same `ApprovalView` the document rendered.**

- The provider renders from a snapshot `{workspaceId, hash, view, version}` taken from the reducer's `approval` entity, versioned with the state (the same rule that stops a late event reviving a request, frontend §6.5a). The approve command reads the workspace id and hash **only from the active document's snapshot**. It never reads them from the entity at send time, and never from command arguments (§8 rule 2).
- If the entity's request changes while the document is open (a new hash, or none), the document re-renders from the new snapshot and any input box in progress is cancelled. The operator must read again and type again.
- A race the client cannot see (the request changed after the last event it applied) is the server's `409 approval_stale`. The extension then refetches the workspace and re-opens the document on the new request, with the server's sentence: *the configuration changed after it was shown, so nothing was approved*.

This is the web card's property (it sends `req.hash` from the object it rendered) made explicit, because in the extension the text and the confirmation are separate pieces of UI.

**What makes approving a hash of a subset you see only as a diff safe.** The operator approves the hash of the whole requested subset but is shown only its difference from the repository's current approval. That is sound only while the baseline the difference was computed against is still the current approval when the hash is approved. Today two facts keep it so:
- **One workspace per repository.** `workspace.ErrInProgress` refuses a second workspace for a repository that already has one, in any state (`internal/workspace/workspace.go`). So no other workspace's approval of the same repository can land between this request's display and its approval.
- **Approving clears the pending request.** The move out of `stopped` takes the request away, so a request is approved at most once.

Neither fact is about approvals, and a change to either (several workspaces per repository is a natural feature) would silently let an operator approve settings they were never shown. **The insurance, recommended as an E1 server change (§11):** the pending request records the hash of the approval it was diffed against (none for a first approval), and `Approve` refuses with `approval_stale` when the repository's current approval is no longer that one, inside the same transaction that already re-checks the request's hash. That costs a column in `pending_approval`'s JSON and one comparison, and turns an accidental property into a checked one. *As built (PR 5):* the request's `baseline` is that hash (`internal/workspace.PendingApproval`, set by step 3), compared inside `Approve`'s transaction with the repository's current approval; a mismatch is `approval_stale` with nothing recorded (`internal/provision/baseline_test.go`). A request stored before the field existed reads as `""`: approved only while the repository has no approval, refused otherwise.

#### As built (PR 5)

`extension/src/review.ts`. The document is `drydock-approval:/<workspace id>`, provided by a `TextDocumentContentProvider` and opened beside the editor; its text is `renderReview`: the title, the repository, branch, workspace and request hash, the warning with the root sentences that apply, and the groups — all from core's `approvalSections`, every repository-written string escaped. A `Reviews` object keeps each open document's snapshot `{workspaceId, hash, view, version, fullName, branch}` and re-takes it only when the entity's request changes (another hash, or none): then the document is told to re-render and an input box open for it is cancelled (`Prompter.input`'s `cancel`). `drydock.approve` reads the workspace and hash from the active document's snapshot alone; with no review document active it opens the pick and the document, never the box. After the box answers it checks the snapshot is still the one it showed. `approval_stale` refetches the workspace, re-takes the snapshot and shows the document again with the sentence; `forbidden_scope`, or a token whose `GET /api/meta` scopes lack `approve`, offers *Review in Drydock*. An accepted approval is followed, as a clone is, to running and *Connect*. The editor title of a review document carries *Approve Host Access…* and *Cancel Host-Access Request* (`resourceScheme == drydock-approval`); the hub's item for a waiting workspace, the tree's, and a follow that ends at a request open the document, and nothing else approves. `drydock.reviewApproval` takes a workspace id (it reads); `approve` and `decline` are mutating and take none. Decline is `Prompter.confirm`, a modal. Its primary button is *Cancel the request*, so **Enter declines** (VS Code's modal makes the first non-Cancel button the default, §10.4 above). That is accepted, unlike for approving, because declining is the safe direction: nothing is recorded, the workspace stays stopped, and Start asks again; a stray Enter costs a restart, never host access. A follow started by an approval passes over the request it approved only while the approval's mark is in flight and not yet slow (`SLOW_AFTER_MS`); past that, a request still showing ends the follow with a sentence and *Open in Drydock*, rather than *Approved; continuing…* forever.

#### Who may approve

- **Only from local and SSH windows** (§8 rule 1). An attached container's extension host cannot approve its own workspace's host access, or any other's, through our commands. That is the attack the approval gate exists to stop: a container granting itself host access. Rule 1 is the control, and the typed name is depth.
- **Arguments are ignored** (rule 2): a caller cannot pre-select a workspace, a hash or a subset. `drydock.approve` with no active review document opens a pick of pending requests and then the document, never the input box.
- **Only with `approve`** in the token's scope (§4.1). Without it, the document's action is *Review in Drydock*, and the server would refuse anyway (`403 forbidden_scope`).
- The approve and decline marks end on `workspace.job` `approve` and on the request disappearing (`settlesApprove`, `settlesDecline`), as in the web UI.

### 10.5  Parity: every web UI surface, and where it lands

The owner asked for eventual parity. This is the inventory (`web/src/routes.ts`, `web/src/components/`), so a gap is a row, not an oversight.

| Web UI surface | Extension | Phase |
|---|---|---|
| Sign-in (`/signin`), the sign-in notice of failed attempts | Not in the extension: it never signs in with the password. *Sign In* pastes a token minted in Settings (§4.2) | T + E1 |
| Settings: *Tokens* (new in T) | *Create a token in Drydock* opens it; the extension's *Sign Out* revokes its own token; expiry warning in the status bar | T + E1 |
| Home: the catalog, Clone per row, catalog refresh, the installation's *add repositories* link | Hub and *Repositories* tree, `clone`, `refreshRepositories`; the installation link via `openExternal` | E1 |
| Home: running workspaces, card status (`lib/workspaceCard.ts`) | Hub and *Workspaces* tree, same status text from core | E1 |
| *Open in VS Code* | `connect` (§7) | E1 |
| Workspace page: state, the eight-step timeline | Progress notification while following; a tree item's children for steps | E1 |
| Workspace page: the activity feed (last 50 events) | A read-only virtual document, *Show Activity* | E2 |
| Start, Stop, Rebuild, Delete (typed name), *Make room* at the cap | Commands, §8 rules 1 and 4 | E2 |
| Session server state, *Restart session server*, the environment link (`claude.ai/code?environment=…`, `Capacity: N/4`) | Tree item detail, `restartSession`, the link via `openExternal` | E2 |
| Logs (`/ws/:id/logs`), build log | Virtual documents | E2 |
| Resources: memory and disk per card, the disk banner | Tree item description (*"mem 1.2 GB · disk 3.4 GB"*, unknown is *"—"*); a status-bar warning at the disk limit | E2 |
| Fleet identity banner, *Check now* | Status bar (§6.3), `checkClaudeLogin` | E2 (live only with `drydock.watch`, §5.5) |
| Claude login handshake (Settings) | `signInToClaude` | E3 |
| Ports panel: list, enable, disable, hide, relabel, host header, rescan, probe, diagnosis | `drydock.ports…`; diagnosis sentences from core's `lib/portDiagnosis.ts` | E3 |
| Preview link | `openExternal`; the browser's own preview handshake | E3 |
| Settings: signed-in devices, *Sign out* (this device), *Sign out everywhere* | Web-only: they are browser sessions, and every session route is outside any token's scope (§4.1) | never |
| Host-access approval (`HostAccessApproval.vue`): the request, the warnings (`privileged`, `initializeCommand`), *Approve and continue*, *Cancel* | The review document from core's `lib/approval.ts`, typed-name approve, modal decline (§10.4) | **E1** |
| Secrets (`/secrets`, new, edit, grants) | *Open in Drydock* only | never, by recommendation (§10.6) |

### 10.6  Secrets: web-only

The owner's decision on approvals does not extend to secrets, and nothing argues for moving them. The value would be typed into the editor, where clipboard managers, extensions that observe input and settings sync are all nearby, and the web UI's form is the place this repository has made safe (frontend §6.4). Reading secret *metadata* (names, reach, grants) would be harmless but is not useful enough to justify the scope. An extension token never carries a secrets scope (§4.2).

## 11. Required server changes

| Change | Phase | Where |
|---|---|---|
| `GET /api/meta` → `{version, api, credential}`, `AuthRequired`, scope `read`. `version` threaded from `main.version` into `config.Config`. | T | `internal/api` route table + handler, `cmd/drydock`, overall §5 |
| **Phase R**: `internal/events/kinds.go` (`type Kind`, every emitted kind), the ~40 constants and the literals migrated, `NewEvent`/`Emit` taking a `Kind`, `TestKindRegistry` | R | `internal/events` and every emitting package |
| `TestExtensionContract` over `extension/contract.json` | E1 | `internal/api` |
| Release: `extra-files`, the `.vsix` asset, `verify`'s list | E1 | `release-please-config.json`, `.github/workflows/release-please.yml` |
| Not the server, but in the same E1 core PR: the web card renders from core's `lib/approval.ts`, and repository-sourced strings have Unicode format characters escaped (§10.4) | E1 | `web/src/core`, `web/src/components/HostAccessApproval.vue` |
| Recommended: a pending request records the hash of the approval it was diffed against, and `Approve` refuses (`approval_stale`) when the repository's current approval is no longer that one (§10.4) | E1 | `internal/workspace/approval.go` |
| **Phase T**, all of §10.0a: `api_token`, bearer `Authenticate`, the `Scope` column and step, `forbidden_scope`, the four token routes, `Alive` for credentials, the token as `approved_by`, *Sign out everywhere* as its own route, Settings' *Tokens* | T | `internal/auth`, `internal/api`, `internal/store`, `web/`, overall §4, §5, §13.2, CLAUDE.md |



Nothing in E1 or E2 needs a new mutating route, a new event or a change to an existing response.

## 12. Testing

The owner's worry is right: IDE extensions are usually tested by a UI robot clicking through a real editor, and that is slow, flaky and mostly tests the editor. This plan follows testing §3.1 instead: **put each assertion in the cheapest tier whose reach includes the boundary it is about**. For this extension most boundaries are not the editor. They are the HTTP protocol, the reducer, the manifest, and the one call that hands VS Code a URI. A UI robot is left with the single thing only it can see.

### 12.1  The tiers

| Tier | Real | Substituted | Lives in | Budget |
|---|---|---|---|---|
| **unit** | The pure modules: command logic over ports, the SSE parser, the token's shape check, the origin binding, link → URI, setting validation, the `api` range decision, `mayMutate` | `vscode`, by not being imported. HTTP, by a fake transport. | `extension/src/**/*.spec.ts` (Vitest, node) | < 10 s |
| **core** | The shared reducer and marks | As today | `web/src/core/**/*.spec.ts`, run by web's `npm run check` | unchanged |
| **manifest** | `extension/package.json` against the source's command registry | Nothing to substitute | `extension/test/manifest.spec.ts` (Vitest) | < 1 s |
| **client** | The extension's HTTP client and SSE reader (no VS Code) against **real Drydock behind real Caddy** on the shipped Caddyfile, with a test CA | GitHub (`githubtest`, in-process), the devcontainer CLI and Docker (fake binaries on `PATH`), Claude (`fakeclaude`) | `test/extension/client/*.spec.ts` (Vitest, node) | < 60 s |
| **extension host** | A pinned VS Code, headless under xvfb, against the same stack. One run on **the bundle unpacked from the `.vsix`** in Test mode, one on **the installed `.vsix`** in Production mode | Remote-SSH and Dev Containers (absent). `vscode.openFolder` is observed, not performed. | `test/extension/host/` (`@vscode/test-cli`) | < 5 min |
| **keyboard smoke** | Real keystrokes into a real VS Code window, the installed `.vsix` in Production mode | As above | `test/extension/ui/` (`wdio-vscode-service`) | nightly, non-gating |
| **Spike 05** | The VS Code behaviour this design assumes and has not measured (§12.9) | Nothing | `docs/design/spikes/harness-05-vscode/` | once, then on each VS Code bump |
| **rituals** | Everything: the laptop, Remote-SSH, Dev Containers, the real server | Nothing | `test/rituals/vscode-*.md` | by event |

### 12.2  Unit, and how the `vscode` boundary is enforced

The extension has two layers. **`src/adapter/`** is the only code that imports `vscode`: registering commands, the quick-pick and input-box wrappers, `SecretStorage`, tree providers, `openFolder`. It is thin enough to read in one sitting (a target of ~400 lines), with no branches worth a unit test. **Everything else** is pure and talks to the adapter through narrow interfaces (`Prompter`, `Secrets`, `Opener`, `Clock`, `Transport`, `Window` for `remoteName`).

Three things make the boundary hold:

1. **Compilation.** `src/tsconfig.pure.json` covers everything but `adapter/` with `types: []`, so `@types/vscode` is invisible there and `import * as vscode from 'vscode'` is a compile error. `npm run check` runs it.
2. **Execution.** The `vscode` module does not exist outside the editor (`@types/vscode` is types only), so a pure module that reached it would fail its own Vitest spec at import.
3. **A named rule.** `boundary.spec.ts` lists every import of every non-adapter file and fails by file on `vscode`, `node:child_process` (the extension spawns nothing, so there is no argv sink to sweep), or any `rejectUnauthorized` or `NODE_TLS_REJECT_UNAUTHORIZED` text anywhere in `src/`. It asserts the scan saw more than zero files and found the adapter's `vscode` imports, which is the positive control. Mutation-checked by adding a `vscode` import to `src/link.ts`.

Specs worth naming:

- **`mayMutate`**: true for `undefined` and `ssh-remote`, false for `attached-container`, `dev-container`, `tunnel`, `codespaces`, `wsl` and an invented name, the last being what proves the allowlist fails closed. And **every mutating command consults it**: the command registry marks each command `mutating` or not, a spec runs every mutating command's handler with a `Window` reporting `attached-container`, and asserts the fake transport saw no request and the replacement sentence was shown. The control is the same handlers under `ssh-remote`, which do send. Mutation-checked by dropping the gate from `drydock.clone`.
- **The origin binding**: a stored `{origin, token}` whose origin differs from `drydock.url` (another host, another port, `http:`, a case change) sends no `Authorization` and asks for a sign-in. The control: the equal origin sends it.
- **SSE parser**: frames split across chunks, a CRLF stream, a comment-only stream (heartbeat), an **id-only frame** (a hidden session-only event, §4.1: it moves `lastEventId` and dispatches nothing), a `resync` with its id, an unknown kind passing through, and a `data:` split over two lines. Fed the server's real output recorded from the client tier, as testing §7 records Claude's.
- **The token**: the paste is trimmed of surrounding whitespace and nothing else, refused unless it is `drydock_` + 43 base64url characters (the control: a minted token's shape passes), and stored only after `GET /api/meta` answers `200`. A `401` on any route clears it, a `403 forbidden_scope` does not. The expiry warning appears seven fake-clock days before `expires_at`.
- **Link → URI**: every case `internal/vscode`'s tests pin (bare host, user, port, capitals, encoded path), read from **the Go test's own table**, as `lib/capacity.spec.ts` reads `Occupying`. Every shape the web's `vscodeURL` refuses, the extension refuses too.
- **The update URL**: a `meta.version` that is not `^v\d+\.\d+\.\d+$` builds no URL.
- **Command logic over a fake `Prompter`**: the clone flow's every exit (cancelled pick, `409 in_progress`, `409 at_capacity` with its detail, `507 disk_full`, `503 app_not_configured`), each producing the sentence `api/messages.ts` gives the web UI.
- **The approval binding** (§10.4): `drydock.approve` sends the workspace id and hash of the active review document's snapshot and nothing else. With the entity moved on to hash B while the document still shows A, it sends A (and lets the server refuse). It ignores a hash or workspace passed as an argument. A new snapshot cancels an open input box. The typed name is compared exactly: case, whitespace and the short name all refuse. The control: the right name for the snapshot shown sends one `POST`. Mutation-checked by reading the hash from the entity at send time.
- **`lib/approval.ts`** (core): the sections and warnings for each mix of `privileged` and `initializeCommand` (new, changed, only removed, which earns no warning). Values with a newline, a control character and a bidi override render as escaped text. The control: plain ASCII renders unchanged. The web spec and the extension's provider spec both run against it.
- **The stream's holds** (§5.5): a follow keeps the stream past five idle minutes on a fake clock, through a reconnect, and releases it on `running`, `failed`, approval or `workspace.gone`.

### 12.3  The manifest is a tested artifact

Most "does the palette and keyboard work" questions are questions about `package.json`, and they need no editor:

| Assertion | Control |
|---|---|
| Every `contributes.commands` id is `drydock.*`, has category *Drydock*, and is registered by `activate` (the source's command registry list, which the adapter iterates) | The registry and manifest sets are equal and non-empty |
| Every command is in the palette: none is hidden by `menus.commandPalette` `when: false` unless an allowlist names it with a reason | One allowlisted entry exists, so the allowlist path is live |
| Every keybinding names a contributed command, has `mac` and default keys, and its `when` parses (VS Code's `when` grammar, a small parser in the spec) and uses only context keys the source sets or VS Code documents | A deliberately broken fixture manifest fails each check by name |
| `extensionKind` is exactly `["ui"]`. No `extensionDependencies`, no `extensionPack`. `activationEvents` contains no `*`, and `onStartupFinished` only once `drydock.watch` exists (E2) | |
| **Every `contributes.configuration` key is `"scope": "application"` with `"ignoreSync": true`** (§6.4) | The fixture manifest with `drydock.url` at `machine` fails by name |
| `engines.vscode` equals the pinned test version's minor (`^1.N.0`) | |
| No `contributes.views` of type `webview`, and no webview API in `src/` (§8 rule 5) | |

Mutation-checked by deleting a command from the palette, adding `*` activation, adding `extensionDependencies`, and setting `drydock.url` to `machine`. At runtime the extension-host tier adds the one check the manifest cannot: `vscode.commands.getCommands(true)` contains every contributed id once the installed extension has activated.

### 12.4  The client tier: the protocol against the real server, without VS Code

This is the tier that does the most work, and it is cheap because it is plain Node.

**The stack.** The browser tier's certificate minting, Caddy process and per-run temp root move from `test/browser/harness.ts` to `test/harness/` and are shared by both tiers. The server cannot be the browser tier's `drydock serve`: `serve` has no flag for the GitHub API's base URL (`config.GitHubAPI` is set only by tests), so its catalog is `app_not_configured`, and E1 needs a catalog and a clone that reach `running`. Two facts decide how the stack is built instead:

- `githubtest.New` and `EnableGit` take a `testing.TB` (`internal/github/githubtest/fake.go`, `git.go`).
- `server.New` builds one `container.Manager` and hands *copies* to the provisioner and to the `vscode.Linker` (`internal/server/server.go`). So the walking-skeleton test's trick of setting `srv.Provisioner.Containers.Run` afterwards (`internal/server/workspace_test.go`) never reaches the link, and a clone's view would carry none.

So the stack is **a long-running `go test`**: `test/extension/stack/stack_test.go`, `TestServeStack`, skipped unless `DRYDOCK_EXTENSION_STACK` names a config file, which the harness writes. Inside it, `githubtest` gets its `testing.TB`. It sets `cfg.GitHubAPI` before `server.New`, and `Provisioner.Cloner.BaseURL` after it (`Cloner` is a pointer, so that assignment does reach the cloner). It replaces **no Go seam for containers**. Instead, `devcontainer` and `docker` are **fake executables first on `PATH`**, since `subproc.Exec` resolves both by name, so every copy of the manager runs them. That is the browser tier's stand-in `docker` extended, plus a stand-in `devcontainer` that answers `read-configuration` and prints the one JSON object `up` prints. One seeded repository, `drydock-fixture/dind` (an installation account no real owner has, so a fixture name can never be mistaken for, or collide with, one of the owner's repositories in a log or a ritual), gets a configuration asking for `privileged` (and a second variant adding `initializeCommand`, switched by a file the harness can rewrite), so step 3 stops it for approval as a real docker-in-docker repository would. The stack's own test is the control that the stop happens. It also tests the argv Drydock really builds, which CLAUDE.md prefers to a Go mock. It serves on the real Unix socket, behind the real Caddy on `deploy/Caddyfile`, until the harness stops it. The harness runs it with `go test -timeout 0` (the default 10-minute timeout would kill a long session), and the test `signal.Notify`s for `SIGTERM` and *returns* when one arrives, so `t.Cleanup` runs: the server shuts down, `githubtest`'s server closes and the temp root is left in a state the sweep can read. An un-handled `SIGTERM` would kill the test binary with none of that.

What the stand-ins must answer is exactly what the eight steps and the link ask: the volume checks, `read-configuration`, `up`, the `ps`/`inspect` that find the container by label and read its name and clone mount, and the `exec` that runs `fakeclaude` for step 8. The stack's own Go test records their argv and is its positive control: the catalog lists the seeded repositories, a clone reaches `running`, and its view carries a link. It does **not** test the docker guard. `--docker-path` names the test binary, which the stand-in `devcontainer` never runs. The guard is the container tier's. This stack is the largest piece of test infrastructure E1 needs, and the plan should be sized for it.

**Trust without turning validation off.** The client tier runs Vitest with `NODE_EXTRA_CA_CERTS=<root>/ca.pem`. Node adds that CA to its default root store at startup, for `https` and `fetch` alike, with validation fully on. The browser tier's rule carries over: never `rejectUnauthorized: false`, never `NODE_TLS_REJECT_UNAUTHORIZED=0`. The control is the browser tier's wrong-host certificate: the same client pointed at a Caddy serving a valid-chain certificate for another name must fail with a TLS error and send nothing (the tap shows no request).

**Name resolution.** The browser tier uses the port-less `https://drydock.test`, with Chromium's `--host-resolver-rules` mapping it to Caddy's port. Node has no such flag. So this tier's UI origin is `https://drydock.test:<port>`, with the stack's `--ui-origin` set to match (`OriginAllowed` compares the port; `HostAllowed` strips it). The shared harness therefore takes the origin as a parameter, and the two tiers do not share one. In CI the job adds `127.0.0.1 drydock.test` to `/etc/hosts` as root before dropping to `pwuser`. Locally `run.sh` refuses with a one-line instruction rather than editing `/etc/hosts` itself. No resolver hook is added to the production client.

**What it asserts**, each with its control in the same test:

| Assertion | Control |
|---|---|
| A mutation without `Origin` is `403 forbidden_origin`. The client always sends it, so this is the test that the client's header is what makes it pass. | The same client's `POST /api/workspaces` → `202` |
| The token goes only to the origin it was stored for. A `3xx` to another origin is not followed with it, and a client configured for the second Caddy site with the first site's stored token sends none (the tap on the second site records no `Authorization`) | The same request to the storing origin carries it (tap) |
| Sign-in → a token minted by the harness (signed in to the web routes, `POST /api/tokens`) and pasted → `GET /api/meta` → `{origin, token}` stored. The extension never calls `POST /api/auth/session` (tap) | The meta answer carries the token's label and scopes |
| Scope: a token without `approve` → `403 forbidden_scope` on the approve route, the token kept | The same request with an `approve` token → `202` |
| A request with a cookie and any `Authorization` header → `401` | Either alone → `200` |
| The bearer stream carries no `secret.*`/`api_token.*` kind, live or replayed after a reconnect across a secret's creation | A cookie client's stream over the same window has them |
| A revoked token ends the stream: *Revoke* in Settings (from the harness's cookie session) → the server's next heartbeat `Alive` check closes the stream (it cannot send a status mid-stream), the client's reconnect gets the `401`, the one 401 path runs, and the stored token is cleared. *Sign Out* revokes the token itself (`DELETE /api/auth/token`), and a later request with it is `401` | Before the revocation the stream delivered events |
| SSE reconnect with `Last-Event-ID`: kill Caddy, restart it, and events written meanwhile arrive once each, in order. Past the replay window → `resync` → refetch | Events before the kill arrived |
| **Clone and follow, end to end**: `POST /api/workspaces` → the mark ends on `settlesClone` → the reducer reaches `running` → the refetched view carries a `vscode.url` that `vscodeURL` accepts | |
| **Real payloads are recorded** for the unit tier's fake transport and the SSE parser's specs, one per kind a scenario produces (the kinds themselves are checked statically against the registry, §9.1) | The recording is non-empty and includes `workspace.state` |
| `GET /api/meta` answers the stack's `{version, api}` | |
| **Approval, as the API does it**: clone `drydock-fixture/dind` → `stopped` with an `approval` carrying `privileged` → `POST …/config-approval` with that hash → `config.approved` with the same hash → `running` | |
| **A stale hash is refused**: decline, switch the fixture to the `initializeCommand` variant, start again → a new request B. Approve with A → `409 approval_stale`, nothing recorded | Approve with B → `202` |

**As built (PR 3).** Where the code settled what this section left open:

- **The specs live in `extension/test/client/`**, not `test/extension/client/`: Vitest resolves `vitest` and the extension's modules from the importing file, and `extension/` is where they are. `test/extension/` holds the stack (`stack/`, the Go test, and `stack.ts`, which starts it) and `run.sh`; `test/harness/` holds the pieces the browser tier now shares (Caddy, the CA, the tap, the process helpers).
- **The stack runs as its compiled test binary** (`go test -c`, then `-test.run ^TestServeStack$ -test.timeout 0`), not under `go test`, so the harness's SIGTERM reaches the test and `t.Cleanup` runs.
- **It does not exercise the docker guard.** `--docker-path` names the stack's test binary, which the fake `devcontainer` never runs, so the guard's argv checks are the container tier's alone.
- **The fakes are the test binary itself**, run through two wrapper scripts on `PATH` that name which (`DRYDOCK_STACK_FAKE`, in `TestMain`): Go rather than shell, and no build of their own. `read-configuration` answers from the corpus's own recordings, docker-in-docker's for the `drydock-fixture/dind` repository (a marker file in its clone). `TestStackControl` runs in the ordinary `go test` as the stack's positive control.
- **The CA is made by Vitest's global setup**, before the workers fork, since `NODE_EXTRA_CA_CERTS` is read once at a process's start; each spec file then starts its own stack around it.
- **The `extension` CI job stays on the bare runner** for now, with Go, the pinned Caddy and the `/etc/hosts` line; it moves into the Playwright image when the extension-host tier needs Electron.
- **A revoked token's stream** is tested by dropping the connection (a Caddy restart) rather than waiting out the server's 20 s heartbeat, which `internal/server`'s tests already hold to `Alive`: what this tier proves is the client's reconnect meeting the `401` and taking the one 401 path.
- **Not yet here:** the replay-window `resync` (it needs an event-log gap the stack cannot yet force), committed recordings for the unit tier (the payloads are written under the run's root), and §12.7's skew rows, which are the extension-host tier's.

The fake transport in the unit tier is checked against this tier: the recorded responses it replays are captured here, so the fake can never claim a shape the server does not produce (testing §6.1, *no fake without a contract test*).

### 12.5  The extension-host tier: commands in a real VS Code

**VS Code itself is pinned and checked.** `@vscode/test-electron` downloads and unpacks VS Code by itself, which leaves no step at which to verify it. So the harness downloads the pinned archive by URL, checks it against a SHA-256 in the repo, unpacks it, caches it by version, and passes `vscodeExecutablePath`, as CI already does for `certutil`'s `.deb`. It runs headless under `xvfb-run`. Each run gets its own `--user-data-dir`, `--extensions-dir` and `--logs-path` under the run's temp root, so the canary sweep (§12.8) has one tree to search. It runs with `--password-store=basic`, since CI has no keyring.

**Same bytes, two modes.** An earlier draft said this tier "tests the `.vsix`". As designed it mostly did not: every spec that signs in needs the Test-mode seam, and that ran from the source tree. Now both runs use the packaged artifact:

- **The Test-mode run** unzips `drydock-vscode.vsix` (`extension/` inside it) and passes *that directory* as `--extensionDevelopmentPath`. The bundle under test is the one the release uploads, byte for byte, and `extensionMode` is `Test`. Every behavioural spec below runs here.
- **The Production-mode run** installs the `.vsix` into the extensions directory. Without the seam it cannot answer an input box, so it cannot paste a token. It asserts what Production adds: activation, `getCommands`, `exports === undefined` and lazy activation. It hosts **no** signed-in case: signed out, `drydock.stop` stops at *Sign In* and never reaches its modal, so a negative test there would pass vacuously. The modal case is the keyboard suite's (§12.6, §12.8). Signing in and connecting in Production mode with real keys is the keyboard smoke test (§12.6).

**Driving commands.** Specs call `vscode.commands.executeCommand('drydock.clone')` and so on. The test seam is narrow and observable:

- In `ExtensionMode.Test` **only**, `activate` returns `{ prompter, opener, window }`. `prompter` is a scripted `Prompter` answering picks, inputs and confirmations. `opener` is a recording `Opener` that stores the URI it would hand `vscode.openFolder` instead of opening a window, since nothing in this host can resolve `ssh-remote+…` (§12.11). `window` lets a spec report a `remoteName`, which is how this tier drives rule 1 without a real remote.
- **The control for the seam itself:** in the Production run, `vscode.extensions.getExtension('krelinga.drydock').exports` is `undefined`. The seam cannot ship live. Mutation-checked by returning the exports unconditionally.
- The real `Prompter` and `Opener` (each a few lines over `showQuickPick` and `executeCommand('vscode.openFolder', …)`) are covered by the keyboard smoke test and the connect ritual, and by a unit test that pins the exact call: command id, URI and `{ forceNewWindow: true }`.

**What it asserts:**

- **Activation is lazy**: after the window opens and no Drydock view is shown, the tap has seen no request. Control: opening the view causes one `GET /api/workspaces`.
- **Sign-in persists across a reload** (`workbench.action.reloadWindow`, then the extension host comes back signed in). That proves the token went to `SecretStorage`. §12.8's sweep proves it went nowhere else.
- **E1's flow from the hub**: `drydock.go` → scripted pick of an unheld repository → clone → follow → the recorded `Opener` URI equals the `vscode-remote://` rewrite of the view's `url`, byte for byte. The stream is still open six fake-clock minutes into the follow.
- **Rule 1 in a real host**: with `window` reporting `attached-container`, every mutating command, run with the scripted operator answering yes to everything, sends nothing (tap) and shows the replacement. The control is the same script under `undefined`, which sends.
- **Every E2 mark ends on its settling event**, including after the stack forces a `resync` (an event-log gap past the replay window). This is the extension's copy of `inflightResync.spec.ts`, run against the real server.
- **Workspace and folder settings cannot move `drydock.url`**: a `.vscode/settings.json` in the test workspace setting it to the second Caddy site changes nothing, and the second site's tap records no request. The control: the same value in user settings does move it, and is then refused the stored token (origin binding).
- **Keybinding conflicts**: read VS Code's default keybindings and parse them. No default binding uses `ctrl+alt+shift+d`. Control: `ctrl+shift+p` is found as `workbench.action.showCommands`. *How* to read them is unmeasured: `openTextDocument(vscode://defaultsettings/keybindings.json)` from an extension may not resolve, and the fallback is `workbench.action.openDefaultKeybindingsFile` and reading the active editor. Spike 05 decides.
- **E1's docker-in-docker flow**: clone `drydock-fixture/dind` → the notification offers **Review** (and no approve) → the review document's text equals core's `approvalSections` for the request, `privileged` sentence included → scripted typed name → the `POST` carries the document's hash → the follow continues to `running` → the recorded `Opener` URI.
- **Rule 1 for approve**: with `window` reporting `attached-container`, the scripted operator typing the right name sends nothing, and the document offers *Do this from a local or SSH window*. The control is `undefined`, which sends.
- **Approve exactly what was shown**: the document is open on A, with the scripted operator at the input box. A second client declines and restarts with the `initializeCommand` variant. When B's event lands, the input box closes and no `POST` is sent, and the document now shows the `initializeCommand` sentence. Typing again sends B's hash. **And the race the client cannot see**: the harness holds the SSE relay at the tap, so the extension has not seen B, and the scripted operator confirms A. The `POST` carries A, the server answers `409 approval_stale`, and the extension refetches and reopens the document on B. The control in the same test is the unheld run approving A → `202`.
- **Version skew** (§12.7).

**As built (4b).** The tier is `extension/test/host/`: `host.spec.ts` (Vitest, outside VS Code) starts the client tier's stack, downloads the pinned VS Code by URL and checks its SHA-256 (`vscode.json`, cached in `~/.cache/drydock-vscode`), and runs `inner/suite.ts` in it with `@vscode/test-electron`'s `runTests`, headless under `xvfb-run` (`test/extension/run.sh host`). The specs inside reach the stack through a loopback control channel (`ctl.ts`: the tap, a mint, a view's link). Where it differs from the plan above:

- **Both runs use the packaged `.vsix`** (PR 6): `run.sh host` runs `npm run package`; the Test-mode run develops the `.vsix` unpacked, and the Production-mode run installs it with VS Code's CLI into its own extensions directory and develops a probe extension (`test/host/probe`), so Drydock's runs in Production mode: installed, lazy, every command registered once activated, and `exports === undefined` (mutation-checked by returning the seam unconditionally).
- **Its CA is in the system store, not `NODE_EXTRA_CA_CERTS`** (Spike 05 #1, measured wrong for the extension host). `test/extension/system-ca.sh` makes and trusts it, as root; CI runs it on a runner that lives for one job, and `run.sh host` refuses to run until the system store verifies the CA, never editing it itself.
- **Covered now:** lazy activation (nothing asked of Drydock until a command; showing the view opens the stream and reads the list), every contributed command registered, workspace settings unable to move `drydock.url` (with `editor.tabSize` from the same file as the control), `http.proxyStrictSSL` not off, sign-in over HTTPS through Caddy, the wrong-host certificate refused by VS Code's own HTTP stack, rule 1 for every mutating command under a reported `attached-container` (the refresh from a local window the control), E1's hub flow to the recorded `vscode-remote://` URI, and the default keybindings' conflict check. **Not yet:** sign-in surviving a reload, the canary sweep of the run root (§12.8), the approval specs (PR 5), skew (§12.7) and the hostile fixture.
- **The CI job stays on the bare runner**, which has xvfb and Electron's libraries; the Playwright image was not needed.

### 12.6  UI-level tests: evaluated, and kept small

| Option | What it is | For | Against |
|---|---|---|---|
| **ExTester** (`vscode-extension-tester`) | Selenium + ChromeDriver against VS Code's Electron, with page objects for the workbench | Mature page objects for quick picks, trees, notifications | Its own VS Code and ChromeDriver downloads per version, page objects that break when VS Code's DOM changes (monthly), Selenium flakiness, minutes per spec |
| **`wdio-vscode-service`** | WebdriverIO service that launches VS Code and exposes workbench page objects | Same reach, a lighter runner, real keystrokes, and it can run the extension-host API from the test | The same DOM churn and download cost |
| **Manifest contract + extension-host tier** (§12.3, §12.5) | No DOM | Covers registration, palette presence, `when` syntax, conflicts with defaults, every command's behaviour, the exact URI | Cannot see a keystroke reach a command, a quick pick render, or the operator answer a real modal |

The contract tier and the extension-host tier cover nearly everything a UI robot would, at a fraction of the cost and with no DOM to drift. What only a robot can see is **real input**. So there is a small `wdio-vscode-service` suite (chosen over ExTester for the lighter runner and real key events), on the **installed `.vsix` in Production mode**:

- *Keyboard-only connect*: sign in by typing the harness-minted token into the paste box, the chord reaches `drydock.go`, typing filters, Enter clones, and the notification's `notification.acceptPrimaryAction` connects. That ends in a window VS Code tries and fails to resolve, since there is no SSH. The spec asserts the attempt from VS Code's log, the one place a Production build reveals the URI it opened.
- *Docker-in-docker by keys*: clone `drydock-fixture/dind`, `notification.acceptPrimaryAction` on **Review**, the document opens, the editor-title action by its command, the name typed, Enter, `running`, connect.
- *The hostile extension against the real modal and the approval input* (§12.8), after the suite's typed token, so both really open.

It runs **nightly in its own workflow** (`.github/workflows/extension-nightly.yml`, in the Playwright image), not inside `stress.yml`: that job runs on a bare runner without Xvfb, and its tests stop at minute 52 of a 60-minute job. It runs on demand too, and is **not a required check**. A flake files an issue the way the stress job does (`flake: extension-ui …`). It is promoted to CI only after a month without a flake. If it never gets there, the connect ritual covers the same path by hand.

### 12.7  Version skew, both directions

- **Unit**: the `api`-range decision over every row of §9.1's table.
- **Newer server, current extension**: the stack sets `meta.API = max + 1` (a field on the handler struct, with no build-time constant to override). The extension-host tier asserts every mutating command is replaced by *Install the matching extension* and sends nothing (tap). Read commands and connect still work, which is the control.
- **Older server, current extension**: the client and extension-host tiers run once against the **previous release's binary**: downloaded by tag, checked against that release's `SHA256SUMS`, and run as the browser tier runs `drydock serve`. It has no `/api/meta`, so `404` → `api 0` → read-only. `serve` cannot be pointed at the fake App, so its repository list is `app_not_configured`, which the extension must show as that sentence. The control is that **the workspace list, the identity and the stream** load from it (the list empty, since without an App it can clone nothing).
- **Older extension, current server**: once a release carries a `.vsix`, the extension-host tier also unpacks **the previous release's `.vsix`** and runs its own E1 specs against today's stack. It must either work fully (within range) or show the update action, and never fail in some third way. This is what checks the server's claim that an additive change did not need an `api` bump.
- **Contract drift**: `TestExtensionContract` for routes and for event kinds against the registry (§9.1), both static, in the ordinary `go test`.

### 12.8  Security tests

**The canary sweep, extended to the editor** (testing §4.2). The canary is **the token**: the harness mints it through `POST /api/tokens`, so it knows the value without reading it back from anywhere. After a full E1 scenario (sign in, clone, approve, connect, sign out, sign in with a second token), the sweep greps the whole run root (user data dir, extensions dir, logs, `globalStorage`, `workspaceStorage`, crash dumps, captured stdout and stderr, the VS Code output channel's log files, and Caddy's and Drydock's logs) for each token and its base64, URL-encoded and JSON-escaped forms:

- **Expected: no hit** for either token in plaintext anywhere, and in the database only as its SHA-256 (the one expected hit, asserted present, as the session hash is in testing §4.2).
- **A positive proof of where the token is.** With `--password-store=basic`, VS Code encrypts `SecretStorage` entries in `state.vscdb` with a fixed key that ships in VS Code itself. The sweep decrypts our extension's entry and finds `{origin, token}` equal to the canary. So "found only in `SecretStorage`" is shown, not inferred from absence. That it can be decrypted at all is the honest statement of what `basic` is.
- **The sweep must find something**: a non-secret marker (the cloned workspace's id), which the extension logs at info level, is found by the same sweep in the output channel's log. Without it, a sweep that searched an empty tree would pass.
- E3 adds the Claude login code as a canary.

**The hostile extension.** A fixture extension, `test/extension/host/fixtures/hostile/`, runs beside ours. Its job is to attack §8's depth rules, since rule 1 is tested directly (§12.2, §12.5):

- It calls every `drydock.*` command with crafted arguments (a workspace id, `{ confirm: "krelinga/alpha" }`) and checks every return is `undefined`.
- **Against the real modal** — in the keyboard suite (§12.6), not the extension-host tier, because it needs a signed-in Production build and only the suite can sign one in (by typing a token). The whole nightly suite runs with `"window.dialogStyle": "custom"` in its user settings, so keyboard-connect never meets a native dialog either, and the modal is DOM the suite can see and dismiss. Custom is also the stronger negative: a DOM dialog is reachable by workbench commands in a way a native one is not, so if nothing accepts the custom one, the native case is the easier one. A native modal under xvfb is a separate GTK window that blocks the workbench and that wdio cannot find, so the run could only end by killing VS Code. The sequence, in one spec:
  1. Signed in, the suite runs the fixture's own command `hostile.attack` from the palette. The fixture fires `drydock.stop` **without awaiting it** (awaiting would run the accepts only after the modal was answered). It then loops over every id in `getCommands(true)` matching `/accept|select|confirm|submit|primary/i` across quick input, dialogs and notifications (`workbench.action.acceptSelectedQuickOpenItem`, `notification.acceptPrimaryAction` and whatever else the pinned version has). It keeps looping, recording a timestamp for each call, until the suite signals it is done (the fixture's `hostile.stop` command, which also returns the call log).
  2. **The modal opened**: the suite finds the dialog in the DOM, naming the workspace, and records when it first appeared. Without this check, a signed-out or broken run would pass the next step vacuously.
  3. **Nothing was sent**: after the dialog appeared, the suite waits two seconds of loop time, then checks the tap has no `POST …/stop`, then signals `hostile.stop`.
  4. **The attack reached the dialog**: the call log shows at least one complete round (every matching id, once) that began after the dialog's appearance. Without it, accepts that all fired before the modal existed would make step 3 vacuous.
  5. The suite presses Escape (*Cancel*): the dialog is gone and the tap is still empty.
  6. **The positive control**: `drydock.stop` again, the suite presses Enter on *Stop*, and the tap records the `POST`.
- Mutation-checked by letting `drydock.stop` honour its argument and skip its modal (step 2 then fails by name).
- **The same spec against approval**, with `drydock-fixture/dind` stopped for approval and its review document active (opened by the suite, as an operator would). The fixture fires `drydock.approve` unawaited, with `{ hash, workspace }` arguments: first a forged hash, then the real one. The test hands the fixture the real one, which makes the test stronger than any real attacker could be. It loops the accept-like commands, plus `type` with the repository's full name followed by each accept, timestamping each call, until the suite signals. The suite asserts:
  1. the approval input box opened (in the DOM);
  2. at least one full round landed after it appeared;
  3. the tap has no `POST …/config-approval`;
  4. Escape leaves it so.

  The positive control: the suite types the name and presses Enter, and the tap records the `POST` with the document's hash. Mutation-checked by accepting the hash argument. If Spike 05 finds `type` can fill another extension's input box, rule 1 is still the control, and this case says so by failing.
- This proves "no command accepts the modal" for the custom dialog style only, and nightly only. The native style is Spike 05 #4's, measured on the laptop, where a blocked window can be closed by hand. On a PR, the depth rules are checked only by the Test-mode run's argument and return-value cases. The window rule (§8 rule 1), which is the primary control, is checked on every PR.

**TLS and `Origin`**: the wrong-host certificate and the `Origin` pair live in the client tier (§12.4). The extension-host tier repeats the wrong-host case once with VS Code's own HTTP stack in play, since VS Code patches Node's `https` and `fetch` for proxies and system certificates. That run also asserts `http.proxyStrictSSL` is not `false` in the test profile, since a test profile that disabled it would quietly pass the wrong-host case.

### 12.9  Spike 05: what this design assumes about VS Code and has not measured

Every item below is a belief about undocumented or unmeasured VS Code behaviour that the design leans on. Spike 05 (`docs/design/spikes/05-vscode-extension-host.md`, with `harness-05-vscode/`) measures each against the pinned VS Code and the current Remote-SSH and Dev Containers, and is re-run on each bump (§12.11's bump ritual). The items needing a real container window run on the laptop. The rest run in the CI image. It is the first task of E1's test work.

| # | Belief | Leaned on by | If wrong |
|---|---|---|---|
| 1 | `NODE_EXTRA_CA_CERTS`, set on the launched VS Code, reaches the extension host's `https` and `fetch` | §12.5's trust | **Fallback, CI only:** the test CA in the CI container's system store (`/usr/local/share/ca-certificates`) with `http.systemCertificates` on. Local `run.sh` refuses rather than editing a long-lived devcontainer's trust store, as it refuses for `/etc/hosts`. Either route is paired with the wrong-host control |
| 2 | A command invoked from a container window's remote host runs in *that window's* local instance of our extension, where `remoteName` is the container's | §8 rule 1 | Rule 1 would be judging the wrong window. The design would need a caller check VS Code does not offer, and mutations would leave the extension until there is one |
| 3 | `remoteName` is `attached-container` (and `dev-container`) in the windows Connect and *Reopen in Container* open | §8 rule 1 | Harmless by construction (an allowlist), but the replacement sentence would show in the wrong windows |
| 4 | No workbench command accepts a modal, for both `window.dialogStyle` values | §8 rule 4 | Rule 4 is no depth at all, and rule 1 carries everything, which the design already assumes |
| 5 | Whether a remote host can run `workbench.action.terminal.newLocal` then `…terminal.sendSequence`, i.e. **a container window can run code on the laptop** | §2.3, §4.5, §8.2 | If it can, it is recorded as the residual risk of every attach (§8.2), reported to the owner, and weighs on open question 1 |
| 6 | `application`-scoped settings are ignored in remote and workspace settings, and remote settings cannot move `http.proxy`, `http.proxyStrictSSL` or `http.systemCertificates` for the local host's `fetch` | §6.4 | If remote settings move the `http.*` keys, the client builds its own TLS agent and ignores them |
| 7 | VS Code's default keybindings can be read from an extension (`vscode://defaultsettings/keybindings.json`) | §12.5 | The `openDefaultKeybindingsFile` fallback |
| 8 | `notification.acceptPrimaryAction`'s default key on each platform | §6.2 | The hub's *ready* row is the keyboard path, and the notification only a convenience |
| 9 | VS Code's test resolver (`vscode-test-resolver`, in VS Code's own repository and not shipped) can be built and loaded against the pinned version, giving CI a **real remote extension host** | Would upgrade §12.5's rule-1 and settings tests from a reported `remoteName` to a real remote, with the hostile fixture running *in* it and a remote settings file | They stay as designed: a reported `remoteName`, workspace settings, and the connect ritual's real container window |

### 12.10  Invariant → test map

| Invariant | Tier | Test | Control |
|---|---|---|---|
| The extension invents no state: entities come only from the reducer | core + unit | The extension has no entity setter. `boundary.spec.ts` fails on any write to `entities` outside core | Reducer specs move state |
| A mark ends on its settling event, never the `202` | extension host | §12.5, with a forced `resync` | The mark exists before the event |
| A follow holds the stream until it ends | unit + extension host | Fake clock past five minutes | The stream closes once the follow ends |
| `Origin` is the UI origin, and nothing else gets it | client | §12.4 | `202` with it |
| The token goes only to the origin it was stored for; no ambient credential | unit + client + extension host | Origin binding; cross-origin tap; workspace settings cannot move the URL; no webview (§12.3) | The storing origin gets it |
| A token reaches only its scopes; `session` routes never; unset scope closed | server meta-test (T) + client | Every route walked with and without its scope; an unset scope fails the table; approve without `approve` → `403`; `AuthRedirect` refuses a bearer | With the scope, the handler runs |
| A token's stream shows only kinds within its scope | server (T) + client | `secret.*` and `api_token.*` events absent from a bearer stream and its replay | A cookie stream receives them |
| Settings a repository can write cannot redirect the extension | manifest + extension host + Spike 05 | `application` + `ignoreSync`; workspace settings ignored; remote settings (§12.9 #6, #9) | User settings do move it |
| The token (and later the login code) never written outside `SecretStorage` | extension host | Sweep, plus decrypting the `basic` entry | Sweep found its marker; the entry holds the canary |
| TLS validation never off | unit + client + extension host | `boundary.spec.ts` text rule; wrong-host cert refused | Right-host cert accepted |
| No mutation outside local and SSH windows (rule 1, primary) | unit + extension host | `mayMutate` table; every mutating handler consults it; a reported `attached-container` sends nothing | The same under `undefined` and `ssh-remote` sends |
| Commands cannot be driven by another extension (depth) | extension host (arguments, returns) + keyboard smoke (modal) | Hostile fixture with every accept-like command, after the modal is seen open | The operator's real Enter on the modal, in the same spec (nightly) |
| Approve only from local and SSH windows | unit + extension host | `mayMutate`; a reported `attached-container` with the right name typed sends nothing | `undefined` sends |
| Approve exactly what was shown | unit + client + extension host | Snapshot-only hash; held-stream race → `409`; a new request cancels the input | The shown hash approves |
| The extension shows what the web card shows | core | Both render `lib/approval.ts` | The web spec's sentences |
| No one-keystroke approve | manifest + keyboard smoke | No keybinding or hub item for `drydock.approve`; hostile `type` + accept sends nothing | The operator's typed name sends |
| Lazy activation, no request without use | extension host | Tap empty after start | View open → request |
| The link is the server's, rewritten only in scheme | unit + extension host | Go table cases; recorded `Opener` URI | |
| Skew is detected in both directions | client + extension host | §12.7 | In-range server works fully |
| The release ships what CI tested | CI `extension` job + release `verify` | The job tests the `.vsix` it builds from the release commit; `verify` checks the asset's hash, version and manifest | `jq .version` equals the tag |

### 12.11  What CI cannot test, and the rituals that do

**The real attach cannot run in CI.** It needs a real SSH host with Docker, Remote-SSH and Dev Containers in the profile, and a way to observe the window they open, which the extension that asked for it does not have. (The extensions' licence restricts them to Microsoft's builds, which CI's pinned VS Code is, so the licence is not the obstacle.) So CI stops at the boundary and asserts it exactly: the URI and the command handed to VS Code (§12.5). Everything past that is a ritual, a file in `test/rituals/` with a checklist and commands (testing §11):

- **`vscode-connect.md`** — the E1 done-when on the real laptop and server: install the release's `.vsix`, mint a token in Settings (with `approve` ticked), paste it, and, with the keyboard only, clone `krelinga/drydock-testbed-a`, follow it, connect. Then do the same for a docker-in-docker repository of the owner's: Review, read, type, `running`, connect, and in the container `docker run hello-world` works (the approval did what it said). In the new window: the folder is the clone, `git push` of a `drydock/` branch succeeds through the broker, *Reopen in Container* of the clone on the host finds Drydock's container rather than building one (design §6), and Settings' *Tokens* shows the token's *last used* moving. Then, from that container window: a mutating command says to do it elsewhere (§8, rule 1). And with `drydock.url` set to another origin in the container's `customizations.vscode.settings` (and the workspace rebuilt), the extension still talks to the right Drydock. Run before each release that changes `extension/`, and as part of the release smoke test (testing §11.4).
- **`vscode-bump.md`** — triggered by a bump of the pinned VS Code, or when Remote-SSH or Dev Containers update past the version `internal/vscode` was read from (0.470.0): re-run Spike 05, re-read the attached-container authority in the Dev Containers extension (the procedure `internal/vscode`'s comment describes), re-run the connect ritual, and move the pins in the same commit.
- **`vscode-keymap.md`** — triggered by changing a default keybinding or bumping the pinned VS Code: on macOS and Windows, open *Keyboard Shortcuts*, search the hub's chord and `notification.acceptPrimaryAction`'s, and confirm only the expected command holds each. Check the OS's own shortcut list too.

### 12.12  CI and the nightly run

- **`go` job**: `TestExtensionContract` runs in the ordinary `go test ./...`. `TestServeStack` skips there (no `DRYDOCK_EXTENSION_STACK`). Its fakes are exercised by the `extension` job.
- **`web` job**: unchanged. `npm run check` now covers `core/` and its boundary spec.
- **A new `extension` job** in `.github/workflows/ci.yml`, in **the same Playwright image the `browser` job uses**, pinned by the same digest. It already carries Node and Xvfb. Electron's system libraries overlap heavily with Chromium's, but that overlap is unmeasured, and the job's first run will show what, if anything, is missing. Apt on the bare runner is what drove the browser job into that image. The job adds Go and the pinned Caddy as the browser job does, then, as root, writes the `/etc/hosts` line and, if Spike 05 says so, the CA. Then, as `pwuser` via `setpriv`, it runs `extension/`'s `npm ci && npm run check` (types, unit, manifest, boundary, bundle budget), then `test/extension/run.sh` (the stack, the client tier, the `.vsix` build, both extension-host runs). The checked VS Code archive is cached by version. **Not a required check for its first month**, then added to the ruleset.
- **Nightly**: `extension-nightly.yml` runs the keyboard smoke suite (§12.6) and files `flake: extension-ui <spec>` issues the way `stress.yml` does. Once the extension-host tier is required, a loop of its specs joins that workflow. A timer-driven SSE client is exactly the kind of code a loop exists to shake.
- **Release**: `test` is unchanged. `assets` builds the `.vsix`. `verify` checks its hash, version and manifest, and runs no Electron (§9.2).

## 13. What this deliberately gives up

- **No reach beyond where Caddy is reachable.** The LAN, or the tailnet (§2.1). No relay through SSH.
- **No auto-update** without the operator pressing *Install*. Skew is detected and named instead (§9).
- **No password sign-in in the extension.** The operator mints a token in the browser once per laptop (§4.2), which is one more step than typing a password, in exchange for a scoped, revocable credential built once.
- **No secrets** in the extension, by recommendation (§10.6).
- **A stricter approval than the web card's.** The web card approves with one click. The extension asks for the repository's name to be typed (§10.4).
- **No proof in CI that a window actually attaches.** The URI handed over is proven exactly, and the attach is a ritual (§12.11).
- **No defence against the container window itself.** The extension keeps its commands and credential out of a container's reach, and claims nothing more (§8.2).
- **No webview UI.** Quick picks, trees, notifications and virtual documents only. That costs some presentation (the step timeline is a progress notification and a tree, not the web UI's timeline) and buys §8's *no ambient credential*.

## 14. Open questions

The owner settled the design's questions (Decisions, *Settled by the owner*). What remains open is what only measurement or other work can answer:

1. **Spike 05's results** (§12.9; the report is `docs/design/spikes/05-vscode-extension-host.md`). Measured so far: #1 wrong (the extension host trusts the system store, not `NODE_EXTRA_CA_CERTS`; the fallback is in use), #6's workspace half right, #7 right (`openTextDocument` reads the default keybindings). Still open, for the laptop rituals:
   - #2: a remote caller's command runs in that window's own instance, which rule 1 depends on;
   - #4: what Enter does in a modal;
   - #5: whether a container window can reach a local terminal, which the residual risk in §4.5 and §8.2 depends on;
   - #6: whether `application` scope holds against remote settings.

   Each has its fallback written beside it. A result that contradicts the design comes back here as a decision for the owner.
2. **Two pending dependencies:** PR #141's `initializeCommand` sentence on the web card (open), and #144's root warnings beyond field names. Both are rendered from `lib/approval.ts` once it exists, so the extension inherits them.
