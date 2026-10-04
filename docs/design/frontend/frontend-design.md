# Drydock frontend

*A Vue single-page app, built once and embedded in the Go binary, whose central design rule is that it owns no state machine of its own: every mutation is a `202` and a wait, and the server's event stream is the only thing that ever changes what you see.*

**Status** design document, draft v5 · **Date** 4 October 2026 · §3 and §13.1: `dist/` is committed, guarded by a byte-for-byte rebuild check · §4.5 gains two asks Phase 1 found unmet — per-device revoke and delivering the failed-attempt notice · rebased onto overall draft v7: three §4.5 asks are now schema columns and the supervisor state is `waiting_registration`, and §10's tier split now lives in the [testing plan](../testing/testing-design.md) §10.3, whose draft v4 adds a frontend tier and withdraws its `chromedp` fallback · the card carries one environment id and a capacity fraction, a bad login code retries in place (§6.1, §6.2, §6.6, §9) · wireframes for §6 are Figs 3–6, distilled from `prototype/prototype.html` · supplements the [overall design](../overall/drydock-design.md) (§4 schema, §5 API, §8 sessions, §10 secrets, §13 auth), [port forwarding](../port-forwarding/port-forwarding-design.md) (§6 ports API, §10.5 phishing) and the [testing plan](../testing/testing-design.md) (§10 browser tier)

**Runtime** no runtime. A `dist/` directory in `go:embed`, served from the same Unix socket as the API

**Reach** phone, tablet, laptop on the LAN, behind Caddy — mobile-first, not mobile-tolerant

**Out of scope** a web terminal · a session browser · a file browser or editor · theming · i18n · offline use

## 1. Problem & goals

The overall design says the UI is "a list and a button" and then, correctly, spends its pages on the parts that are hard: the broker, the supervisor, the login handshake. What it leaves implicit is that **the UI is where every one of those mechanisms becomes legible or doesn't**. Ten workspaces going dark because one shared credential was blanked (§7.1) is an architectural footnote and a 2am support call; which one it becomes is a frontend decision.

Five flows carry essentially all the value. Everything else in this document exists to serve them.

| Flow | Where it happens | What makes it hard |
|---|---|---|
| **Glance.** Is anything broken, and what is running? | Home list, from a phone, in five seconds | Two state machines per workspace (§4 `workspace` and `supervisor`) must collapse into one honest badge |
| **Clone.** Pick a repo, tap once, watch it come up. | Home list → workspace detail | Three minutes of build with nothing to show but events; a double tap must not make two clones |
| **Hand off.** Open the workspace in the Claude app. | Workspace card → `claude.ai/code?environment=env_…` | The link is useless if the supervisor is `awaiting_login`; the card has to say which thing to fix |
| **Sign in to Claude.** The §7.2 handshake. | A dedicated view, usually on a phone | It spans an app switch — the page may be discarded between showing the URL and pasting the code |
| **Stop something.** Free capacity, or clean up. | Card action → confirm | Destructive, and the live session count is the only thing that makes it an informed choice (§12) |

### Functional requirements

- **One home screen** that answers "what is running, what is wrong" above the fold on a 360 px viewport, and holds the repo catalog below it.
- **Live without refreshing.** Every state change arrives on the SSE stream; nothing polls, and nothing requires a reload to become true.
- **Survive the network.** A dropped stream reconnects, replays what it missed, and says so — it does not silently show stale state.
- **The login handshake, reliably.** Including the case where the operator leaves the tab to authorize and comes back to a reloaded page.
- **Secrets with the friction intact.** The `reach` field and the grant decision are the security control (§10.4); the form is where that control actually lives.
- **Ports without a prompt.** Ambient counts, decisions made in a panel the operator opened (port forwarding §8.2).
- **Say what §12 says.** Every failure mode in the overall design has a specific sentence. The UI uses that sentence, not a generic one.

### Non-functional targets

| Dimension | Target | Why this number |
|---|---|---|
| Initial JS, gzipped | < 100 KB | Vue + router + store is ~40 KB of it. The rest is a budget, enforced in CI, so the list screen never grows a chart library. |
| First meaningful paint | < 1 s on a four-year-old phone | Served off the LAN by a Go binary, so the network is free and the only cost that can grow is parse and hydrate. |
| Smallest supported viewport | 360 × 640 | A phone in a case in one hand. The design target, not the degraded case. |
| Browsers | Last two majors of iOS Safari, Chrome, Firefox | `__Host-` cookies, `SameSite`, `EventSource`, and `dvh` units all assume current-ish. iOS Safari is the binding constraint. |
| Largest list | ~50 repos, ~15 workspaces (§1) | Two orders of magnitude below needing virtualization. Plain `v-for`, and the budget above is what keeps it that way. |
| Offline capability | None, deliberately | §7. A cached shell for a tool whose entire content is live server state is a liability. |
| Build output | One `dist/`, embedded | No Node on the dev server, no second thing to supervise, no static-file route to misconfigure. |

### Non-goals

- **Not a terminal.** §1 of the overall design already rules this out. Interactive work happens in the Claude app; the UI shows status and the one handshake that cannot be automated.
- **Not a session browser.** §8 is explicit: a count and one link. The Claude app is better at browsing sessions and keeping a mirror of remote state accurate buys nothing.
- **Not an iframe host.** No preview is ever embedded in the UI. §8 explains why this is a security decision and not a layout preference.
- **Not themeable, and not localized.** One operator, one language, two color schemes driven by `prefers-color-scheme`. A theme picker is a setting to maintain for an audience of one who already has an OS-level one.
- **Not a component-library showcase.** About twelve components carry the whole app. §3.1 argues that is below the threshold where a framework pays for itself.
- **Not server-rendered.** There is no crawler, no cold-start SEO concern, and one authenticated user. SSR would add a Node runtime to a design whose shipping story is a single static binary.

## 2. Binding constraints

The frontend analogue of §2. Five facts, none of them a preference, and the first one determines the shape of everything else.

### 2.1  Every mutation is `202`, so the client has no idea what happened

> [!WARNING]
> **Load-bearing constraint**
>
> §5: *"Every mutating call is asynchronous: it validates, writes a state transition, returns `202` with the workspace, and lets the client follow along on the stream."*

A `202` is not a result. It means the request was accepted and that the consequences will arrive later, out of band, on a stream that may not even be connected at that moment. So the reflex most Vue apps are built on — mutate, await, patch the store from the response, render — is unavailable here, and faking it is worse than not having it.

This kills the obvious implementation: a store where `stopWorkspace()` sets `state = 'stopped'` on success. That store would be lying a quarter of the time (the stop can fail at the container step), and it would diverge the moment a second device did anything. The rule that replaces it is §4's whole subject: **the frontend invents no state.** A click produces exactly one piece of local state — *a request is in flight* — and everything else is rendered from what the stream said.

The payoff is worth stating because it is the reason this is a feature rather than a tax: **the UI renders your own click through exactly the same path as another device's.** There is no second code path to keep consistent, no "did I do this or did my laptop" ambiguity, and reloading mid-operation loses nothing.

### 2.2  The session cookie is invisible to JavaScript

`__Host-drydock` is `HttpOnly` (§13.2), which is correct and means the app cannot ask whether it is signed in. There is no token to inspect, no expiry to read, no decode. The only way to know is to make a request and see whether it comes back `401`.

So authentication state is *derived from traffic*, not stored: the app boots by calling `GET /api/auth/session`, and a global fetch wrapper treats any `401` from anywhere as the authoritative "you are signed out now" — including the `401` that arrives mid-session when the 14-day idle window lapses while the tab was in the background.

### 2.3  `EventSource` cannot tell you why it failed

This one is a genuine trap. `EventSource.onerror` carries no status code and no body. Worse, the two failures that matter look nothing alike in consequence but arrive at the same callback:

| What happened | `readyState` after the error | Right response |
|---|---|---|
| Wifi dropped, Caddy restarted, laptop slept | `CONNECTING` — the browser is already retrying | Nothing visible for a few seconds. Then a quiet "reconnecting" marker. Never a modal. |
| `401` — the session expired | `CLOSED` — per spec a non-200 fails the connection permanently and does **not** retry | Classify it with an ordinary `fetch`, then show sign-in |

Reading `readyState` distinguishes them, and a probe request confirms it. Getting this wrong produces one of two bad apps: one that flashes a scary banner every time a phone changes cell, or one that sits on a dead stream showing confidently stale state forever. §4.3 specifies the handling.

The second half of the same constraint: a reconnect is a *gap*, and events that occurred during the gap are gone unless the server replays them. `EventSource` sends `Last-Event-ID` automatically if the server sets `id:` on each event — which the schema already supports, since `event.id` is an autoincrementing integer. §4.5 makes that a requirement rather than an accident.

### 2.4  The login handshake spans an app switch

§7.2 step 3: the operator authorizes in *their own browser* and gets a code. On a phone, that is a tab switch or an app switch, and iOS Safari is free to discard the backgrounded page. So any design that keeps the handshake's identity in a JavaScript variable — or in `sessionStorage`, which survives less than people assume — produces a UI that comes back from the authorize step with no idea what it was doing, holding a code it cannot submit.

The fix is the same rule as 2.1, applied to the handshake: the server owns it, at most one is in flight, and the client re-reads it (§4.5). The client persists nothing. Notably it must not persist a *draft of the code* — a one-time credential in `localStorage` is precisely the artifact §13.5's redaction rule exists to prevent.

### 2.5  No secret, token, or value ever comes back

§13.5: *"No route returns a secret value. Not for an edit form, not for a 'reveal' button, not behind a re-auth prompt."* This is a frontend constraint as much as an API one, because the pressure to break it comes from the UI side: an edit form naturally wants to prefill, and a rotation naturally wants to show what it is replacing.

It does neither. The secret form is write-only, the rotate form opens empty and says so, and there is no component in the app that can render a secret value because none is ever in the store. The same applies to GitHub tokens (there is no route) and to the Claude login code (it goes up, never comes back down).

## 3. Shipping shape

One Vite build, output embedded. Nothing is served from a CDN, nothing is fetched at runtime from anywhere but Drydock's own origin, and there is no Node process on the dev server.

```
web/                           # not shipped; built
  index.html
  src/
    main.ts                    # mount, router, store install
    app.vue                    # shell: nav, banner slot, <router-view>
    routes.ts
    api/
      client.ts                # the fetch wrapper: 401, Origin, error envelope
      stream.ts                # EventSource lifecycle, replay, classify
      types.ts                 # generated from the Go types; see §4.5
    stores/
      session.ts               # auth state, device list
      catalog.ts               # repos + joined workspace state
      workspaces.ts            # workspace detail, supervisor, ports, events
      secrets.ts
      identity.ts              # Claude login state — fleet-wide (§6.6)
      stream.ts                # owns the EventSource; the only writer
    components/                # ~12; see §6
    views/
    styles/
      tokens.css               # the design system, such as it is (§3.1)
      base.css

internal/web/
  embed.go                     # //go:embed dist
  dist/                        # build output, committed (see below)
```

**`dist/` is committed** (settled in v5, when Phase 1 built it). The constraint was always that **the Go build must not require Node**, and of the two answers that honor it, a committed `dist/` is the one that also keeps `go install`, a plain `go build`, and the release workflow free of a Node step — the release builds from the tag with Go alone. The cost of committing build output is drift: a binary could ship a UI no source in the repository describes. `npm run check:dist` closes that by rebuilding into a scratch directory and failing on any byte of difference, and CI runs it on every pull request. Never hand-edit `dist/`; change the source and rebuild. A `go generate` that shells out to `npm` remains ruled out.

#### Serving it

Three rules, and the second one is the bug everyone writes once.

1. **Hashed assets are immutable.** Vite emits `app-4f3c1a.js`; serve those with `Cache-Control: public, max-age=31536000, immutable`.
2. **`index.html` is `no-store`.** It is the only unhashed file and it names the hashed ones. A cached `index.html` after a Drydock upgrade requests assets that no longer exist, and the app fails to boot in a way that looks like a server problem.
3. **The SPA fallback must not swallow `/api`.** Unknown paths under `/api/` return a JSON `404`; unknown paths elsewhere return `index.html`. Falling back on `/api/typo` hands the fetch wrapper an HTML document to parse as JSON, and the resulting error names neither the route nor the typo.

`/preview/authorize` (port forwarding §6) is a server handler on this mux, not a client route — it issues a `302` and must never reach the SPA fallback.

### 3.1  Choices worth defending

- **Framework — Vue 3, Composition API, `<script setup>`, TypeScript.** Requested, and a good fit: the app is mostly one list and one detail view reacting to a push stream, which is reactivity's home turf. TypeScript is not ceremony here — the store is a reducer over a dozen event kinds against a normalized entity map, which is exactly the code where a wrong field name fails silently and late. Types are generated from the Go structs (§4.5) so the contract has one source.
- **Build — Vite, single bundle, no SSR.** Two lazy chunks (secrets, logs) and nothing else split; at this size route-level splitting costs more in round trips than it saves in parse.
- **Store — Pinia, with one store owning the stream.** Not because the app needs a state-management library for its size, but because §4's architecture is "one writer, many read models", and Pinia makes that shape explicit and testable. The reducer is the single most test-worthy unit in the frontend (§10) and it wants to be a plain function in a store, not logic smeared across components.
- **Router — `vue-router`, history mode.** History rather than hash because the UI is a real origin with a real certificate and deep links into a workspace are genuinely useful — "the build failed, look" is a URL you paste to yourself. The cost is the fallback rule above, which is five lines.
- **Styling — plain CSS with design tokens, scoped per component. No utility framework, no component library.** The honest comparison: twelve components is roughly six hundred lines of CSS, which is less than the configuration and class-soup surface of the alternatives, and it reads as a diff. Tailwind's productivity argument is real but it is a velocity argument, and the binding constraint here is *legibility to whoever (or whatever) reads this code in six months*, where one tokens file beats utility classes inlined across thirty templates. A component library (Vuetify, PrimeVue, Quasar) is ruled out harder: all three cost more in bundle than the entire budget in §1 and all three impose a mobile idiom rather than letting §7 pick one.
- **Fonts — the system stack, no webfont.** `system-ui, -apple-system, Segoe UI, Roboto, …`, which is also what the design doc's diagrams use, so the UI and its documentation look related. Zero network, zero layout shift, and nothing to add to the CSP.
- **No runtime dependency beyond `vue`, `vue-router`, `pinia`.** Everything else — date formatting, the few hundred bytes of relative-time logic, the clipboard call — is written. Three dependencies is an auditable surface for an app behind the only credential standing between a device on your wifi and code execution on your dev server (§13.5).
- **Color scheme — `prefers-color-scheme`, no toggle.** Light and dark token blocks, same convention as the diagrams.

### 3.2  The loop everything goes through

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/01-mutation-loop-dark.svg">
  <img alt="A tap on Clone issues a POST which returns 202 Accepted carrying no UI state; the only local state is that a request is in flight. The server writes a state transition, which appears on the SSE stream as an identified event, which a store reducer applies, which renders the card as cloning. The same path renders a click from any other device." src="diagrams/01-mutation-loop-light.svg" width="100%">
</picture>

**Fig 1** — *The `202` is a receipt, not a result. Between the tap and the first event the UI knows one thing — a request is outstanding — and it says exactly that. Because the render path begins at the event and not at the response, the UI behaves identically whether the click came from this device, the laptop in the next room, or a `curl` on the host; and a reload mid-operation loses nothing, because there was nothing local to lose.*

## 4. State architecture

### 4.1  One writer

The stream store is the only thing that writes entity state. Everything else reads.

```
EventSource ──▶ stream store ──▶ reducer ──▶ normalized entities
                     │                          │
POST/PATCH ──────────┘ (in-flight set)          └──▶ catalog / workspaces /
                                                      secrets / identity (read models)
```

| Store | Owns | Written by |
|---|---|---|
| `stream` | The `EventSource`, connection phase, last seen event id, the in-flight request set, the normalized entity maps | Itself, from events and from request lifecycle |
| `catalog` | Repos joined to workspace state; search and sort for the home list | Read model over entities; refetched on `repo.*` events |
| `workspaces` | The detail view's composition: supervisor, environment id and capacity fraction, ports, recent events, disk | Read model; fetches `GET /api/workspaces/:id` on entry, then lives off the stream |
| `secrets` | Names, reach, grants, last access. Never a value. | Fetch + `secret.*` events |
| `identity` | Claude login state for the whole fleet, and the in-flight handshake | Fetch + `auth.*` events |
| `session` | Signed-in-ness, device list | `GET /api/auth/session`, and any `401` from anywhere |

Entities are normalized by id — `workspaces: Record<string, Workspace>`, `repos: Record<number, Repo>` — so an event naming one workspace touches one key and every view of it updates. Lists are arrays of ids computed in the read models.

### 4.2  The action lifecycle, and the state it is not allowed to invent

Every mutating action follows the same four steps, and the third one is the discipline:

1. **Mark in flight.** `stream.begin(key)` where `key` is e.g. `workspace:01J…:stop`. The button reads this and disables, showing a spinner — not a label.
2. **Send.** `POST`, expect `202`. A non-202 is an error (§9); a `409` from the API is specifically "something already in progress", which is a message and not a failure. (Do not confuse it with the `409` *Claude Code* returns to a restarting supervisor — that one is a 60–200 second wait, it surfaces as a workspace state rather than a response, and §6.1 is emphatic that it must not read as a failure either.)
3. **Do nothing with the response body.** The `202` carries the workspace, and it is tempting. It is also already stale by the time it is parsed, and applying it introduces a second write path for entity state. Discard it, or use it only to confirm the id you already had.
4. **Clear in flight when the state actually moves,** i.e. when an event for that workspace arrives — not when the response lands.

> [!NOTE]
> **The timeout that must not be a failure**
>
> If no event arrives within a few seconds of the `202`, the operation is still running — the server accepted it. So the in-flight marker stays, and after about ten seconds the UI adds a quiet *"no response yet"* beside it. It never flips to an error, and it never re-enables the button, because the one thing that is certainly true is that a stop was accepted. Timing out into a failure state here is how a user ends up pressing stop twice.

Three things are legitimate local state, and they are the whole list: the in-flight set, transient view preferences (sort order, which sections are collapsed, whether hidden ports are shown — `localStorage`, non-sensitive, disposable), and the content of a form field the user is currently typing into.

### 4.3  Reconnect, replay, and classify

```
connect ──▶ open ──▶ live ──┬──▶ error, readyState CONNECTING ──▶ degraded ──▶ (browser retries) ──▶ open
                            │                                       └─ after 5 s: show "reconnecting"
                            └──▶ error, readyState CLOSED ──▶ probe GET /api/auth/session
                                                                ├─ 401 ──▶ signed out
                                                                └─ ok  ──▶ hard retry with backoff
```

On every `open` after the first, the client does two things:

- **Trusts the replay for the gap.** The browser sent `Last-Event-ID` automatically; the server replays events with a greater id (§4.5). Those flow through the normal reducer, so the gap closes with no special case.
- **Resyncs as a backstop.** Refetch whatever the current route needs — the catalog, or the open workspace. Replay is bounded and a long sleep can outrun it; the server says so with a `resync` event, but refetching on reconnect regardless costs one request and removes an entire class of "the page was wrong after my laptop woke up" bug.

*As built (Phase 2):* the backstop is not optional, because a **hard retry** after a `CLOSED` stream is a new `EventSource`, and a new one sends no `Last-Event-ID` — only the browser's own reconnects do, and the API cannot be given one in a header. So that path has no replay at all and relies wholly on the refetch. The refetch is handed to the reducer as a *snapshot* tagged with the last event id seen when it was requested: a workspace an event newer than that has touched keeps the event's state, anything else takes the snapshot's, and every field carries the id that last wrote it, so a replayed or late event at or below it is a no-op. A `resync` keeps the entities on screen, marks them *may be out of date* in the header, and is cleared only by a snapshot taken at or after it.

The visible treatment matters as much as the mechanism. A phone switching networks produces a `CONNECTING` error constantly, so **nothing appears for the first five seconds.** After that, a small marker in the header — not a toast, not a modal, nothing that moves the layout — and the data on screen stays visible and is not greyed out. Stale-but-labelled beats blank.

### 4.4  The `401` path

Any `401` from any request, at any time: `session.signOut()`, which clears entity state, closes the stream, and routes to `/signin` with the current path kept as `return`. After a successful sign-in the router restores it.

Entity state is cleared rather than kept, deliberately. The alternative — sign back in and find the old data still rendered — is friendlier and wrong: between the `401` and the new sign-in the server may have been restarted, workspaces may have been reconciled (§6 of the overall design), and the one state the UI must never show is a confident rendering of a world that moved on.

### 4.5  What the API must add

The routes in §5 of the overall design are a backend contract and mostly complete. A UI built on §2.1's rule needs nine additions, listed here rather than there so that table stays the server's own. None is a redesign; three of them are the difference between a correct UI and a plausible one.

| # | Addition | Why the UI cannot work without it |
|---|---|---|
| 1 | ✅ *Built in Phase 2: see overall §5.* **`GET /api/events` sets `id:` on every event**, supports `Last-Event-ID` replay from a bounded window, emits a `resync` event when the requested id has fallen out of that window, and sends a `: ping` comment every ~20 s. | §2.3. Without ids there is no replay and every reconnect is a silent gap; without `resync` the client cannot tell a closed gap from an unclosed one; without the heartbeat a dead-but-open connection looks live. |
| 2 | **`GET /api/auth/claude` returns any in-flight login** — `login_id`, phase, the scraped URL, the deadline. ~~And reports `blanked` and `absent` distinctly~~ — **settled in v7**: `claude_identity.state` is a stored column holding exactly `ok\|expiring\|expired\|blanked\|absent`, so the route has the five-way verdict to return and the classification happens once, in the poller. Only the in-flight login half remains. | §2.4 and §7.3. The handshake must be recoverable after an app switch. The identity half was the more important ask and it is now schema, not prose — which matters because `auth status --json` reports `loggedIn:false` for blanked and missing alike, so a derived-per-reader verdict would have differed between the banner and the card. |
| 3 | **`POST /api/workspaces` rejects a second create** for a repo that already has a workspace in a non-terminal state, with `409`. | A disabled button is not a concurrency control. Double-tap on a phone is a real input, and two clones of one repo is a wasted three-minute build plus a confusing list. |
| 4 | **`GET /api/workspaces/:id/logs?tail=n`** reads the supervisor ring buffer (§8), redacted, never persisted. | §14 Phase 6 promises a log viewer and §5 has no route for it. The buffer is in process memory by design, so the UI has no other way in. |
| 5 | **`PUT /api/secrets/:name`'s stale-workspace list says which *kind* of stale** per workspace: `new_commands` or `needs_supervisor_restart`. | §10.3 requires the UI to distinguish these and says Drydock knows from the resolved configuration. The UI must not infer it — guessing wrong here produces exactly the twenty minutes of confusion the §10.3 warning is about. |
| 6 | **`GET /api/auth/session` marks the current device** with `is_current`. | So the device list's revoke button can warn that this one is you, rather than signing you out as a surprise. |
| 7 | **A consistent error envelope** — `{"error":{"code":"…","message":"…","detail":"…"}}` — with a stable machine-readable `code`. | §9 maps the §12 failure modes to specific sentences. The alternative is matching on prose, which breaks the first time a message is reworded. |
| 8 | **`GET /api/workspaces/:id` includes whether the resolved config declares MCP servers**, and the installation-settings URL appears in `GET /api/repos`. | The first drives #5's message on a live workspace; the second is what makes §9.4's "link straight to the installation settings page" a link rather than a sentence. |
| 9 | **`GET /api/workspaces/:id` returns the capacity fraction (`used` / `total`), and for a refused supervisor start the matched signature** — not the raw message. ~~And the `environment_id`~~ — **settled in v7**: it is a `workspace` column, described there as "the card's only link". | §6.1. The card renders a capacity fraction, so it needs both numbers rather than a session list. And §8 gives four refused-start causes behind one exit code, each needing a different card message; classifying them from prose in the client is the string-matching §9's error envelope exists to avoid. |

Two more surfaced when Phase 1 built the device list and the sign-in screen against the real route table, and neither is a route yet:

| # | Addition | Why the UI cannot work without it |
|---|---|---|
| 10 | **Revoke one device**: a `DELETE` on a single session by its id. Today `DELETE /api/auth/session` signs out the current device, or with `?all=true` every device. | §5's device list exists so a lost phone can be signed out. With only "this one" and "all", the operator's only move against one lost device is to sign out everything — workable, and what the Settings screen offers until this lands, but not the design. |
| 11 | **The failed-attempt notice reaches the client.** The server already counts bad-password attempts since the last successful sign-in, and their sources (`auth.SignInResult`), but the sign-in `POST` answers `204` with no body, so the count is computed and dropped. Either a body on that response or a field on `GET /api/auth/session`, shown once. | §8's last bullet and overall §12: a stale saved password on a forgotten device shows up only here. A count nobody sees is the same as no count. |

Three of these went from asks to schema in v7 — `claude_identity.state`, `workspace.environment_id`, and `supervisor.state`'s `waiting_registration` value — and the overall design's own reasoning for all three is the one this document argues from: *"a state the prose requires and the schema cannot hold is a state that gets inferred differently by every reader."* A UI is simply the reader where that divergence becomes visible.

TypeScript types for all of this are **generated from the Go structs**, not hand-written. Two hand-maintained copies of a contract between two languages in one repository drift, and the drift shows up as a field that is `undefined` at runtime and fine at compile time.

## 5. Screen map and navigation

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/02-screen-map-dark.svg">
  <img alt="On a phone the app is a single column: a header with a fleet-wide banner slot, a Running section of workspace cards, an All repositories section with search, and a three-item bottom tab bar for Workspaces, Secrets and Settings. On a desktop the same components reflow into a left navigation rail with the list and the workspace detail side by side, so the detail is a route on mobile and a column on desktop." src="diagrams/02-screen-map-light.svg" width="100%">
</picture>

**Fig 2** — *One component tree, two layouts. The workspace detail is a route on a phone and a column on a desktop, which is a layout decision rather than two views to keep in sync. The `Running` section exists because the home screen's job is "what is wrong" and a catalog of fifty repos answers a different question — with fifteen workspaces and fifty repos, putting the catalog first means scrolling past forty-five idle rows to find the one that is building.*

| Route | Screen | Notes |
|---|---|---|
| `/signin` | Password, and the failed-attempt notice from §12 | The only route reachable without a cookie. Carries `return`. |
| `/` | Home: `Running` cards, then `All repositories` with search | The glance flow. Deep-linkable, but it is the default. |
| `/ws/:id` | Workspace detail: state, sessions, ports, events, actions | A route on mobile; a column beside the list at ≥ 900 px. |
| `/ws/:id/logs` | Log viewer (lazy chunk) | Separate route so a 200-line buffer is never in the detail payload. |
| `/secrets` | Secret list with reach, grants, last access | Lazy chunk. |
| `/secrets/new`, `/secrets/:name` | Create / rotate / grants | §6.4. |
| `/settings` | Claude identity, device list, capacity, refresh catalog | Where `GET /api/auth/claude` and the device list live. |

Navigation is three destinations, which is what makes a bottom tab bar the right mobile idiom rather than a hamburger: **Workspaces · Secrets · Settings.** At ≥ 900 px the same three become a left rail. There is no nested navigation anywhere, and no modal that can be navigated out from under.

## 6. The components that are more than glue

Most of the app is a card, a badge, a button, and a list row. Six things are not, and they are where the design doc's reasoning either survives into the product or is quietly lost.

> [!NOTE]
> **Four of the six are drawn; all six are clickable**
>
> `prototype/prototype.html` is a working harness for every surface in this section at 360 px, with a switch for the fleet-wide login state so §6.6's override can be watched rather than argued. It is the thing to open first; the figures below are distilled from it.
>
> Two surfaces are deliberately *not* diagrammed. The **secret form** (§6.4) and the **destructive confirm** (§6.5) are ordinary forms whose entire design is which words appear and whether a field is required — a wireframe of either would show a labelled textarea and teach nothing the prose does not. Drawing them would be decoration, which the repo's diagram convention is meant to exclude.
>
> One palette note: these figures add a desaturated brick (`#9C3729` light / `#E08C7C` dark) to the three colors the existing diagrams use. The card is the first diagram in this repository that has to separate *degraded* from *failed*, and amber cannot carry both.

### 6.1  The workspace card: two state machines, one badge

§4 of the overall design gives a workspace seven states and its supervisor six, and they are independent. A card that renders them as two badges pushes the join onto the reader at exactly the moment they are least able to do it, which is while something is broken. So the card renders **one** line of status and **one** primary action, derived from the pair.

That the sixth supervisor value is called `waiting_registration` rather than `failed` is a schema decision made for this card's benefit: v7 added it so that, in the overall design's words, *"the `409` wait in §8 is not stored as a failure."* The UI is downstream of that choice rather than compensating for its absence, which is the right direction — a UI that has to infer "this failure is actually a wait" from an error string is a UI that will get it wrong once and then stay wrong.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/03-card-states-dark.svg">
  <img alt="The workspace card anatomy and five of its states. One enlarged card shows its parts: the repo name in mono, one status line derived from the workspace and supervisor state pair, a detail line carrying the step or error or capacity, and one primary action. Below, five miniature cards show running-serving with a capacity fraction and an Open in Claude action, running-waiting saying it is waiting for the previous server with no action, running-awaiting-login asking for a Claude sign-in, failed naming the build step with a Rebuild action, and stopped noting the clone is intact with a Start action." src="diagrams/03-card-states-light.svg" width="100%">
</picture>

**Fig 3** — *The card has room for one status line and one action, and that scarcity is the design. Reading left to right along the bottom: the capacity fraction is actionable where a bare count is not; `waiting_registration` offers nothing to press because pressing is the mistake; `awaiting_login` points at a fleet-wide fix rather than a local one; `failed` names the step; and `stopped` says what survived. The grey pair above each card is the state it came from — it would not ship.*

| `workspace.state` | `supervisor.state` | The card says | Primary action |
|---|---|---|---|
| `pending` / `cloning` / `building` | — | The step, from `state_detail`: *"Building image…"* | none |
| `running` | absent | *"Container up, no session"* | Start session |
| `running` | `starting` | *"Starting session…"* | none |
| `running` | `awaiting_login` | *"Claude is not signed in"* | **Sign in to Claude** — see §6.6 |
| `running` | `waiting_registration` | *"Waiting for the previous session server to release the folder"* + elapsed | none — **not a failure** |
| `running` | `serving` | *"Capacity 1 / 4"* + the environment link | Open in Claude |
| `running` | `degraded` | *"Session degraded"* + `last_error` | Restart session server |
| `running` | `exited` | *"Session stopped"* | Start session |
| `stopped` | any | *"Stopped"* | Start |
| `failed` | any | *"Failed while "* + the step that failed | Rebuild |
| `deleting` | any | *"Deleting…"* | none |

Four details that are not cosmetic.

**`running` + `serving` is the only combination that shows the link** — §1's whole promise is a session you can drive from your phone, and a link that opens a session server which is not serving is a worse outcome than no link.

**The link is one environment, not a session.** Spike 02 settled this: the server advertises a single `claude.ai/code?environment=env_…` per workspace and reprints `Capacity: N/4` on every repaint, so the card needs no session enumeration and `rc_session` never has to be accurate for the card to be right. The card shows the capacity fraction rather than a bare count, because the denominator is the thing that makes it actionable — *"Capacity 4 / 4"* is why a new session from the phone will not start. Note the pre-created session counts toward it, so `--capacity 4` buys three on-demand ones; the card must not imply otherwise by showing *"3 sessions"* when one of the four slots is the primary checkout.

**`waiting_registration` is not `failed`, and the design says so in those words.** A `SIGKILL`ed server with no live session blocks the next start with a `409` for a measured 60–200 seconds. That is a wait, it is retried on a flat interval, and it is explicitly not charged to the crash-restart budget — so the card shows elapsed time and no action, and never the word *failed*. This state exists on the card for one reason: it is the window in which an operator who sees "failed" will start pressing rebuild on a workspace that is about to recover on its own.

**`failed` names the step**, because §6 writes an event for every step precisely so that the UI can; "failed" alone throws away the only thing that makes a rebuild an informed choice. The same applies one level down to a refused supervisor start, which §9 breaks into its four distinct causes rather than one message.

The card also carries, when known: disk usage, the §7.3 expiry warning dot, and the ambient port count from port forwarding §8.2 — *"4 listening · 1 previewed"*. Nothing else. The card is read at a glance from a phone; everything that is not read at a glance belongs in the detail view.

### 6.2  The login handshake view

The most intricate screen, for the reason §2.4 gives. It is a linear flow with a visible deadline, and its state lives on the server.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/04-handshake-loop-dark.svg">
  <img alt="The login handshake as a state machine. Absent leads to starting when the operator signs in, then to awaiting code once the authorize URL is scraped off the PTY. A valid code of the form code-hash-state reaches signed in. An invalid code leads to an invalid-code state which loops straight back to awaiting code, because the process stays at the paste prompt with the same URL still valid and the countdown still running, so no teardown is needed. Only the five-minute deadline exits to timed out." src="diagrams/04-handshake-loop-light.svg" width="100%">
</picture>

**Fig 4** — *The arc back from `invalid_code` is the whole point of the diagram. Only two things leave `awaiting_code` for good — a valid code and the deadline — so a bad paste is a loop rather than an exit, and the form, the PTY, and the URL all survive it. The state machine a UI reaches for by instinct has a terminal failure node here, and that instinct would discard a live login for a half-copied code.*

| Phase | Shows | Recovery |
|---|---|---|
| `absent` | *"Claude is not signed in"* and a Sign in button | — |
| `starting` | *"Starting a login container…"* | Reload re-reads the in-flight login (§4.5 #2) |
| `awaiting_code` | The scraped URL as a big tap target, a Copy button, and a countdown to the five-minute deadline (§7.2) | Same |
| `submitting` | Spinner on the code field | Same |
| `invalid_code` | The error **inline on the still-open form**, countdown still running | Paste again — no restart |
| `ok` | *"Signed in as "* and the expiry | — |
| `timed_out` / `failed` | What happened, and Start over | — |

Six rules on this screen specifically, three of them from Spike 01's measurements:

- **A wrong code is not a dead end.** Spike 01 found the process stays at the paste prompt after `Invalid code` and accepts another attempt with the same URL still valid. So the form stays open, the countdown keeps running, and the error appears beside the field. Tearing the handshake down and making the operator start over — which is what a naive state machine does on any non-success verdict — would throw away a live PTY and a valid URL for the most likely user error there is.
- **Validate the code's shape client-side before sending.** It is `<code>#<state>`, and a truncated copy that lost the `#state` half is the likeliest mistake. `^[^#\s]+#[^#\s]+$` in the form turns a terminal round-trip into instant feedback — *"that looks like only half the code; copy the whole value"*. The server validates it too (§7.2); this is about where the operator finds out.
- **The URL is ~450 characters.** It is a tap target and a copy button, never raw text the operator is expected to read or retype. It must wrap without overflowing a 360 px viewport, and the copy button is the primary affordance on desktop, where the operator may want their other browser.
- **Nothing about the code is persisted.** No draft in `localStorage`, no autofill, `autocomplete="off" autocapitalize="off" spellcheck="false"`, cleared on submit. §2.4. Spike 01 removed the original reason — the prompt does not echo, so the PTY buffer never holds the code — and supplied a better one: Drydock receives the code over HTTP and holds it in memory, where a request log, an error string, or a crash dump can still leak it.
- **The deadline is shown, not implied.** A five-minute PTY timeout the operator cannot see is a flow that mysteriously stops working while they are reading their authenticator.
- **This is not a per-workspace action even when reached from a card.** Login is global to the shared volume (§7.2), so the screen says so — *"signs in every workspace"* — and §6.6 explains why that sentence is load-bearing.

### 6.3  The ports panel

Port forwarding §8.2 is unusually prescriptive about the UI, and all of it is adopted:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/05-ports-panel-dark.svg">
  <img alt="The ports panel, three rows. Port 5173, the vite dev server, is enabled: it shows its full preview hostname and carries both declared and observed provenance flags with an on switch. Port 9090 is bound to loopback, so it is greyed, carries the diagnosis to start it with host 0.0.0.0, and has no enable control at all. Port 8080 is declared but not listening, greyed but present with an off switch. A footer notes that discovery never interrupts: no toast and no badge, because a prompt seen whenever a test opens a socket trains the wrong reflex." src="diagrams/05-ports-panel-light.svg" width="100%">
</picture>

**Fig 5** — *Three rows, three different relationships between "something is listening" and "something is reachable" — which port forwarding §5 keeps as independent columns for exactly this reason. The middle row is the one that earns the panel: a loopback bind is a diagnosis the scan already has, so the fix is printed in the row and the preview is never offered, which means the refused connection never happens.*

- **No prompt, no toast, no badge that demands attention.** A port appearing changes the panel next time it is read. The reason given there is the one that matters: a prompt you see often enough buys habituation rather than safety.
- **Loopback rows are listed, greyed, and have no enable control** — not a disabled one, which reads as "try again". They carry the diagnosis verbatim: *"listening on loopback — start it with `--host 0.0.0.0` to preview it."* The bind address turns the most common failure into something known before anyone clicks.
- **Declared-but-not-listening rows stay visible and greyed**, which is what makes the panel useful on a running workspace whose dev server is not.
- **Provenance is a set, not a choice.** A port can be badged `declared` and `observed` at once; the panel shows both because the declaration supplies intent and the scan supplies truth.
- **Enabled rows show the full preview host**, not a friendly label hiding it. Port forwarding §10.5's residual risk is lookalike phishing, and a UI that hides the hostname it is teaching the operator to trust is working against the only defense there is.
- **Previews open in a new tab** with `rel="noopener noreferrer"`. Never in an iframe — see §8.

### 6.4  The secret form, where `reach` is the feature

§10.4: *"Drydock will not store a secret until you have written down what someone could do with it… a small friction, deliberately placed at the moment you are most able to answer the question."* A form can honor that or defeat it, and the difference is entirely presentation.

- `reach` is a **multi-line field whose visible label is the literal question** — *"What can someone do with this?"* — not a placeholder in a one-line input. Placeholders disappear when you start typing, which is when the question matters.
- **§10.4's four rules of thumb are rendered next to the field**, in the form, at the moment of the decision. They are the actual control; a document nobody rereads is not.
- The **value field is write-only and says so.** On rotate it opens empty: *"The current value is not shown. Saving replaces it."* No reveal, no prefill, no re-auth escape hatch (§2.5).
- **Reserved names are rejected client-side with the reason** — *"disables Remote Control"* for the §2.1 four, *"shadows the `gh` shim's token"* for `GH_TOKEN` — while the server remains authoritative. A client-side check here is not security, it is the difference between learning why at keystroke time and learning that at save time.
- **`all_repos` is a decision, not a checkbox.** It takes a separate confirm naming the count: *"This grants to all 47 repositories."*
- After a successful rotate, the **stale-workspace list is shown with its two kinds separated** (§4.5 #5): *"picks it up on the next command"* versus *"needs a session server restart"*, the latter with the restart button beside it and the §10.3 warning about what a restart ends. Drydock never restarts on its own and the UI never offers to do it for all of them at once.
- **Changing a grant is not a rotation, and the UI must not imply it needs a restart.** Spike 03 confirmed `CLAUDE_ENV_FILE` runs once per Bash command, and the grant set is resolved in the broker on each call — so adding or revoking a repository's access to a secret reaches the next command with nothing to restart and nothing marked stale. Only a rotated *value* produces the two-kinds list above, and only an MCP server or a background process inside the workspace needs the restart. Showing a staleness warning on a grant change would train the operator to restart sessions for no reason, which is the habit that eventually costs someone an agent mid-task.

### 6.5  The destructive confirm

§13.5 settles the mechanism: typing the repo name, and nothing else — no re-auth prompt, because on a single-operator system that buys habituation rather than safety. The UI's job is to not add friction of its own and to make the one friction effective:

- **A sheet, not a modal dialog.** On a phone, a modal plus the software keyboard plus a text input is the layout that breaks; a full-height sheet with the input above the fold does not.
- **The input is compared to `full_name` exactly**, and the request carries `?confirm=<full_name>` as §5 requires.
- **Stop shows the live session count** and asks for confirmation only when it is not zero (§12). Zero sessions is the common case and should cost one tap.
- **The sheet says what survives.** Stop: *"the clone and its worktrees survive; only the conversation is lost."* Delete: *"the container, the clone, and every worktree."* §8's note that worktrees are created without asking is the reason the delete sentence has to be the longer one.

### 6.6  One fault, ten cards

This is the frontend's answer to the thing §15.1 names as most likely to look like a Drydock bug at 2am: a blanked shared credential takes every workspace down at once, and none of them is at fault.

A naive UI shows ten degraded cards with ten *Restart session server* buttons, every one of which is the wrong action and all ten of which will be pressed. So the Claude identity state is **fleet-wide state that overrides per-card presentation**:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/06-fleet-override-dark.svg">
  <img alt="Two panels compared. On the left, a UI that renders each card independently shows five cards all reading session degraded, each with its own Restart session server button, none of which can work. On the right, the same fault produces one banner reading signed out, sign in again with a single Sign in to Claude button; the four session-dependent cards below it say waiting on Claude sign-in and carry no action, while a fifth card whose image build failed keeps its own status and its own Rebuild action, because that fault has a different cause." src="diagrams/06-fleet-override-light.svg" width="100%">
</picture>

**Fig 6** — *The left panel is not a strawman; it is what you get by rendering each card from its own row, which is the obvious implementation. Two things go wrong at once: five buttons appear that cannot work, and the one card with a genuinely different fault is buried among them. The right panel keeps the override narrow — only what the credential actually broke is replaced, so the build failure keeps its own status and its own action.*

| `GET /api/auth/claude` | The app does |
|---|---|
| `ok` | Nothing. |
| `expiring` | A persistent, dismissible-per-session header banner with the countdown, and a warning dot on every card (§7.3). Cards otherwise behave normally. |
| `expired` | Non-dismissible banner. Every card's session area is replaced with *"waiting on Claude sign-in"* and its restart action is replaced by the one global **Sign in to Claude**. |
| `blanked` | Same, with §7.3's wording: ***"Signed out. Sign in again."*** Not a countdown, not "expired" — the two conditions have different fixes and the UI must not blur them. |
| `absent` | ***"No one has signed in yet."*** The first-run state, and a different sentence again. |

The last two rows are one distinction the API cannot make for the UI and the UI must not collapse. §7.3: `claude auth status --json` reports `loggedIn:false` for a blanked credential *and* for a missing one, and they are told apart by the file — present with empty token strings means **everyone just lost access**, absent means **no one ever had it**. On a first run the second is the expected state and nothing is wrong; on a Tuesday afternoon the first means ten workspaces died in the same second. Rendering both as "not signed in" is how a routine first-run screen and the worst failure in the system end up looking identical.

The rule generalizes: **a fault with one cause gets one message and one button, wherever it manifests.** Per-card actions that cannot work are not shown disabled; they are replaced by the action that can.

## 7. Mobile-first specifics

§1 of the overall design lists a phone as a first-class client, so these are requirements rather than polish.

| Concern | Decision |
|---|---|
| Viewport | `<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content">`. The last value is what keeps the keyboard from overlaying the field being typed into. |
| Height units | `dvh`, never `vh`. iOS Safari's collapsing toolbar makes `100vh` taller than the viewport, which puts a bottom tab bar underneath the browser chrome. |
| Safe areas | `env(safe-area-inset-bottom)` padding on the tab bar, `inset-top` on the header. |
| Tap targets | 44 px minimum, and `touch-action: manipulation` on every button to kill the 300 ms double-tap-zoom delay — which also halves the window in which a double tap becomes two clones (§4.5 #3). |
| Hover | No affordance may exist only on hover. Every action is a visible control; there are no hover-revealed row menus. |
| Modals | None. Sheets that slide from the bottom, dismissible by swipe and by a visible Cancel. |
| Pull to refresh | Not implemented. The stream makes it meaningless, and intercepting the gesture to do nothing is worse than leaving the browser's own. A manual Refresh catalog button lives in Settings for the one thing that genuinely polls (§5's `POST /api/repos/refresh`). |
| Install | A `manifest.webmanifest` with `display: standalone`, icons, and `theme-color`, so add-to-home-screen gives a reasonable app. |
| Service worker | **None, deliberately.** A cached shell for a tool whose entire content is live server state buys nothing and costs a whole class of "I upgraded Drydock and the phone is still running the old UI" bug. §1's offline target is "none" and this is what honoring it looks like. |
| Long content | Build logs and event streams scroll inside their own container, not the page, with the newest at the bottom and an explicit Jump to latest once the user has scrolled away. Never auto-scroll under a reading user. |
| Reduced motion | `prefers-reduced-motion` removes sheet transitions and spinner animation; nothing conveys state through motion alone. |
| Focus | On route change, focus moves to the view's `<h1>`. One `aria-live="polite"` region announces state transitions — *"vite-app is building"* — so the stream is perceivable without sight. |
| Color | Status is never color alone: a shape or a word accompanies every badge. |

## 8. What the frontend is responsible for, security-wise

The app holds no credential, which is most of the answer. The rest is a short list of things it must not do, each of which would undo something §13 bought.

- **No iframes, in either direction.** The UI never embeds a preview, and the UI must never be embeddable. A hostile repo's dev server rendered inside Drydock's own chrome can draw a convincing sign-in form, and port forwarding §10.5 already identifies lookalike phishing as the residual risk of the separate preview domain. Enforced by CSP below, not by convention.
- **`Origin` rides automatically, and nothing may interfere.** `fetch` on a same-origin `POST`/`PATCH`/`DELETE` sends `Origin`, which is what §13.3's exact-match check needs. Two prohibitions follow: never `mode: 'no-cors'`, and no state change may ever be a native `<form>` submission, whose `Origin` behavior is not something to depend on. All mutations go through the one client in `api/client.ts`.
- **`localStorage` holds view preferences and nothing else.** Not a token (there is none), not a form draft, not a login code (§2.4), not a log buffer. Anything whose leak would matter is not in the browser's storage.
- **The UI does not echo what it is handed.** Error `detail` strings are rendered as text, never as HTML, and the log viewer is `white-space: pre-wrap` text — §8's buffer can contain repo content, and §13.5 requires redaction server-side, which the client must not undo by interpreting the result.
- **No third-party anything.** No analytics, no error reporting service, no CDN, no font host. A tool guarding host-level code execution does not make outbound requests to places its operator did not choose. This also makes the CSP trivially strict:

```
Content-Security-Policy:
  default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:;
  connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none';
  form-action 'self'; frame-src 'none'; frame-ancestors 'none'
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
```

`frame-ancestors 'none'` and `frame-src 'none'` are the two halves of the first bullet. `Referrer-Policy: no-referrer` on the whole UI complements port forwarding §7's requirement for the handshake specifically — the UI links out to `claude.ai` and to preview hosts, and neither needs to know where the link was.

- **The sign-in screen reports failed attempts.** §12's last-but-one row: repeated failed sign-ins are usually a stale saved password on a device you forgot about, and you want to see it either way. The count and the source appear after a successful sign-in, once.

## 9. What every surface says when it goes wrong

§12 of the overall design pairs each failure with a specific sentence, and the point of specificity is lost if the UI renders a generic one. The error envelope (§4.5 #7) exists so this is a lookup rather than string matching.

| Condition | What the UI shows, and where |
|---|---|
| Claude login expired, blanked, or never established | §6.6. Fleet banner, per-card replacement, three wordings kept distinct. |
| Supervisor refused to start | Four causes behind one exit code (§8), so four messages. `409` → *"waiting for the previous session server"*, with elapsed time and no action. `Workspace not trusted` → *"container misconfigured"*, pointing at a rebuild, since §11 has `postCreate` write the key. `Unable to determine your organization` → the §6.6 sign-in path, not a restart. `cannot be used with --spawn` → *"Drydock built a bad command line"*, which is a bug report and says so. One message for all four would make the only retryable one indistinguishable from the three that must not be retried. |
| Image build failed | Card: *"Failed while building"*. Detail: the last 50 build lines inline, Rebuild as primary. The clone is retained and the UI says so, because that is what makes rebuild feel safe. |
| Broker socket missing or stale | *"GitHub access unavailable for this workspace"* — never a raw git error. §12 is explicit about this one. |
| GitHub rate limited or App suspended | *"GitHub is refusing requests: "*. Never phrased as a Drydock fault, and never offering a retry that will also fail. |
| Repo removed from the installation | Card badged read-only, with *"unpushed work in the working tree survives"* and a link to installation settings (§9.4, §4.5 #8). |
| A repo the operator expected is missing | An empty-state line under the catalog search: *"Repos appear here when the GitHub App is installed on them"*, linking to installation settings. |
| Concurrency cap reached | The clone button is disabled with the cap in the label, **and the list sorts stoppable workspaces to the top** — §14 Phase 6: the UI shows you which one to stop rather than just saying no. |
| Disk above threshold | Banner with per-workspace disk, list sortable by it. |
| Stream disconnected | §4.3. A header marker after five seconds; data stays visible and labelled, never blanked. |
| `409` on an action | *"Already in progress"* as an inline note, not an error. It means the state machine beat you to it. |
| Rotated secret not picked up | §6.4. The two kinds of stale, separated, with the restart warning attached to the one that needs it. |
| Caddy down | Nothing — the UI is unreachable. Worth noting because the UI must not try to be clever about it: containers keep working and the design explicitly refuses a fallback listener (§13.5). |

Empty and loading states get the same attention as errors: a first-run install with no repos, a repo list mid-refresh, a workspace with no ports and no sessions yet. Each says what will make it non-empty.

## 10. Testing

The [testing plan](../testing/testing-design.md) already owns the tiers that reach a browser, and this section deliberately does not restate them. What it adds is the one tier that plan has no reason to carry: **the frontend's own units, which never start a server.**

| Layer | Tool | What it covers | Whose tier |
|---|---|---|---|
| The reducer | Vitest, plain functions | The single most valuable unit in the frontend: a recorded event sequence in, an entity map out. Includes out-of-order ids, replay after a gap, a `resync`, and an event for an unknown workspace. | **this document** |
| Components | Vitest + Vue Test Utils | The §6.1 state table as a parameterized test — every `(workspace, supervisor)` pair renders one status and one action. The §6.6 overrides. The reserved-name rejections. The §6.2 retry-in-place. | **this document** |
| The API client | Mock Service Worker | The `401` path, the error envelope mapping, and the rule that a `202` body is discarded. | **this document** |
| Budget | `size-limit` in CI | §1's 100 KB. A budget without a gate is a wish. | **this document** |
| Accessibility | `axe` on every route | §7's focus and `aria-live` requirements are testable and will rot without a test. | this document, but it wants a real browser, so it rides the browser tier |
| Flows and cookie semantics | Playwright | Sign-in with `return`, a mid-session `401`, the handshake reloaded between URL and code, `EventSource` replaying a gap, the cross-site preview handshake. | the [testing plan](../testing/testing-design.md) §10.2, items 10–13 |

> [!NOTE]
> **The split, and where it is written down**
>
> **If an assertion needs a server, a cookie jar, or a real navigation, it is a browser test; if it needs only a function and a DOM, it is a frontend test.** That rule, the per-assertion table behind it, and the four UI flow tests the browser tier picked up all live in the [testing plan](../testing/testing-design.md) §10.3 — the tier taxonomy is that document's job, and a second copy here would be the drift this repository's own convention warns about.
>
> Two things changed there because of this document, and they are recorded there rather than argued here. The frontend tier was added to its §3 and joins the fast lane in its §12, since a `jsdom` suite over pure functions finishes before a Docker daemon starts. And its `chromedp` fallback is withdrawn: it existed to avoid a second language, and §3.1 above puts TypeScript in the repository regardless, so Playwright's cost was already paid and it now shares a toolchain with the tier above it.

**MSW plus a scripted event stream is also how the frontend gets built before the backend exists.** A fixture file of events replayed on a timer against a mocked API reproduces a three-minute cold build, a failed build, an `awaiting_login` supervisor, a `waiting_registration` one, each of the four refused-start signatures, and a blanked credential — all of which are tedious or destructive to produce for real, and all of which the UI has to get right. §11's phase 1 work depends on this existing first.

This is the same instinct the testing plan applies to Claude Code's terminal output: a recorded corpus beats arranging the real thing. Here the corpus is cheaper, because an SSE event is a line of JSON rather than a PTY transcript — and `prototype/prototype.html` is already a hand-driven version of it.

## 11. Build plan

Mapped onto §14 of the overall design, so the UI arrives with the thing it displays rather than as a phase of its own. Phase 1's rule — *nothing else gets built until every route without a cookie returns 401* — applies here too: the shell exists to prove the gate.

| Overall phase | Frontend deliverable | Done when |
|---|---|---|
| **1 — Front door** | Vite + embed pipeline, CSP and cache headers, the SPA fallback rule, tokens and base CSS, app shell with nav, sign-in view, the `401` path, device list in Settings. The MSW fixture harness from §10. | You sign in from your phone over HTTPS, the app boots from the embedded bundle, and letting the session lapse in a background tab lands you on sign-in rather than on stale data. |
| **2 — Walking skeleton** | The stream store and reducer, normalized entities, home list with `Running` + catalog, the workspace card's state table, workspace detail, event feed, reconnect and replay. | A clone you start on a laptop renders identically on a phone that was never touched, and a reload mid-build loses nothing. |
| **3 — Credentials** | Mostly invisible: a GitHub-access health row on the detail view, the broker-socket message from §9, the installation-settings links. | A workspace whose socket is stale says *"GitHub access unavailable"* and never shows a git error. |
| **4 — Secrets** | Secrets list, the create/rotate form with `reach` and §10.4's rules inline, grants with the `all_repos` confirm, reserved-name rejection, the two-kinds-of-stale result. | Rotating a secret tells you exactly which workspaces need a restart and which do not, and you cannot save one without saying what it reaches. |
| **5 — Claude** | The login handshake view including app-switch recovery, retry-in-place on a bad code, and client-side code-shape validation; the identity banner's five states; §6.6's fleet override; the capacity fraction and environment link on the card; the `waiting_registration` state and the four refused-start messages. | You complete a login from a phone, switching to another app to authorize; a mistyped code keeps the form open; and a blanked credential produces one banner and one button rather than ten. |
| **6 — Livability** | Stop / rebuild / delete sheets, the cap and disk messaging with stoppable-first sorting, the log viewer route, the §12 message table. | You stop using the terminal to clean up, and the cap tells you what to stop. |
| **Port forwarding** | The ports panel, the ambient card count, loopback diagnosis, enable toggle, full-host preview links. | A dev server on loopback is diagnosed before anyone clicks, and no port change ever interrupts you. |

Phases 1 and 2 are the ones that cannot be reordered: the reducer and the state table are what every later phase renders into.

## 12. What this deliberately gives up

- **No optimistic UI.** A stop takes a moment to visibly become a stop. The alternative is a UI that is right most of the time and lies the rest, and §2.1 explains why there is no third option here.
- **No offline anything.** Close the laptop lid, reopen it, and the page resyncs. It does not work on the train.
- **No session browsing, no file browsing, no terminal.** All three are the overall design's non-goals; the UI does not reintroduce them by accident, and the session link plus the Claude app covers the case.
- **No preview embedding.** Checking a dev server means a new tab. §8 trades the convenience for the one defense against the phishing surface a separate preview domain leaves.
- **A design system of one developer's taste.** No component library means no accessibility audit someone else did, which is why §10 gates `axe` in CI rather than trusting it.
- **No per-device preferences beyond sort order.** Five devices, one operator; a settings sync story would be more machinery than the thing it syncs.

## 13. Open questions

### 13.1  Worth a decision before Phase 1

| Question | Leaning |
|---|---|
| ~~Is `dist/` committed, or built in CI?~~ | **Settled in v5: committed**, guarded by `npm run check:dist` in CI. See §3. |
| Does the home list default to `Running` collapsed or expanded when nothing is running? | Expanded, with an empty state pointing at the catalog. A collapsed empty section on first run reads as a broken app. |
| Tailwind after all? | No, per §3.1 — but the reopen trigger is honest: if the component count passes roughly twenty-five, the tokens file stops being the cheaper option. |

### 13.2  Deferred, and what would reopen each

| Deferred | Reopen when |
|---|---|
| **A theme toggle.** System preference only today. | The operator wants light during the day and dark at night on one device that does not switch on its own. Two lines and a `localStorage` key; it is deferred because it is a setting to maintain, not because it is hard. |
| **Virtualized lists.** Plain `v-for` over fifty repos. | The catalog passes a few hundred rows — which §15.3 of the overall design already identifies as the point where listing every repo stops being free, so the two reopen together. |
| **A real log viewer** with search, follow, and level filtering. | The 200-line ring buffer stops being enough to diagnose a supervisor, which is also the point where §8's "never persist the full buffer" would have to be revisited. The UI is not the blocker. |
| **Push notifications** for a failed build or a dying login. | You miss an expiry and lose a day of unattended work. It needs a service worker, which §7 rules out for good reasons, so this reopens as a pair of decisions rather than one. |
| **Multi-operator UI** — attribution on actions, per-device audit. | §1 says one operator and §13.2 says no RBAC. If that ever changes, the event feed needs a "who" column before the UI needs anything else. |

---

#### What I would revisit first as this grows

The reducer. Everything else here is a layout decision or a sentence that can be reworded, but the reducer is where the design's central claim — that the server owns the state machine and the client only renders it — is either true or quietly not. The failure mode is specific and familiar: one urgent bug gets fixed by patching an entity directly from a `202` response, that works, and six months later nobody can tell which state on screen came from the stream and which came from a guess. The §10 test suite exists mostly to make that patch visible in a diff. If any of the rest turns out to be load-bearing in a way I did not expect, my bet is on §6.6 — the fleet-wide override is the only place where the UI has to understand that several things which look independent are one thing, and that is the kind of understanding that gets dropped the first time a new state is added.
