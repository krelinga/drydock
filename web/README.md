# web/ — the Drydock UI

The Vue 3 + TypeScript + Vite app, embedded in the binary from `internal/web/dist`. The design is
`docs/design/frontend/frontend-design.md`; the clickable prototype is
`docs/design/frontend/prototype/prototype.html`.

```sh
npm ci && npm run check     # types, tests, dist is current, size budget
npm run dev:mock            # the app against mocks/backend.ts
```

## Build output

`internal/web/dist` is **committed build output** — the Go build needs no Node, and
`npm run check:dist` fails when it is stale. Never hand-edit `dist`; rebuild it.

## State: `src/stores`

- `reducer.ts` is the one pure writer of entity state. It applies events, `resync`, and three
  snapshots — `GET /api/repos`, `GET /api/workspaces`, `GET /api/workspaces/:id` — each tagged with
  the stream position it was requested at. Each field carries the id of the event that last wrote
  it, so a replay changes nothing and a late event is a no-op.
- **Only the workspace list may drop a workspace**: the catalog joins just each repo's newest, so
  its silence proves nothing. A detail `404` for a workspace the entities hold asks the list, which
  drops it.
- Each workspace has a step timeline versioned per step, and a feed of its last 50 events merged
  by id.
- `workspaces.ts` holds the fetches and the mutations. A clone is settled by the create's
  `workspace.state` carrying its `repository_id`, because the only workspace id before that is in
  the `202` body, which is never read.
- `stream.ts` owns the `EventSource`, the in-flight set and the refetch hooks, and handles
  reconnects: a quiet marker after 5 s, and on `CLOSED` a session probe, then sign-out or a hard
  retry that resumes with `?last_event_id=`. The first `open` refetches when a snapshot was
  requested before it (the server subscribes before answering). Specs drive
  `test/fakeEventSource.ts`, because jsdom has no `EventSource`.
- The stream store keys its runtime by `toRaw(store)`: Pinia's devtools call actions through a
  fresh `Proxy`, and keying by `this` left every button spinning under `npm run dev:mock`.

### In-flight marks

**An in-flight mark ends two ways, by one predicate.** Each action's end is `OVER.*` over an
`Outcome` (gone, state, stuck delete, failed stop), asked of each event (`settles*` in
`stores/workspaces.ts`) and, after every snapshot (`stream.snapshot`), of the entities — but only
for a mark whose request was accepted (its `2xx`) before that snapshot was requested
(`snapshotTag`), since only such a body can tell *not begun* from *over*. That is what ends a mark
whose settling event fell in a `resync` gap; a snapshot showing the action under way keeps it.
Never clear a mark on the `202` or on the state move written before the action's end. A start
settles when the workspace leaves its build, as a rebuild does, and a failed stop on its
annotation. Settings' *Check now* settles on `settlesCheck` (`stores/identity.ts`):
`auth.identity`, `auth.identity_check_failed` or `auth.identity_checked` — the last is the healthy
press's only answer.

**A workspace job's end settles its press, whatever path it took.** The server ends every
workspace job — create, start, rebuild, approve, stop, delete, supervisor — with one
`workspace.job` `{kind, outcome}` after its other events, and `mutate` adds one clause to each job
action's predicate: `settlesJob(id, kind, since)`, a `workspace.job` of that kind for that
workspace newer than the mark. It changes no entity (the reducer only feeds it), so the `OVER`
predicates still decide the card and still back the snapshot path. Clone (no workspace id) and
decline (no job) keep their own rule; *Check now* and the catalog refresh are not workspace jobs.

### Resources

`stores/resources.ts` and `lib/resources.ts` drive the card's *"mem 1.2 GB · disk 3.4 GB"*
(`components/ResourceLine.vue`) and the disk banner (`components/DiskBanner.vue`). The reducer's
seventh input is the stream's named `resources` frame; resources live beside the workspaces
(`entities.resources`, `entities.hostDisk`) and are versioned by the server's round time, not an
event id. Unknown is *"—"*, never 0; stale says so; memory only for `running`.

## Views, components, lib

- Home (`Running` + the catalog with a Clone or Start per row) and `/ws/:id` (state, the
  eight-step timeline, the feed as text, Start). Sign-in *loads* a `return` the server answers
  (`isServerPath`: `/preview/authorize`, the preview handshake) through `navigation.assign` rather
  than routing to it, which would render the not-found view.
- `components/ActionButton.vue` is frontend §4.2 for **every** mutating button: in flight until the
  settling event, *"no response yet"* after 10 s and never a failure, refusals by code,
  `in_progress` a note. A workspace's card action (Start, Stop, Rebuild, *Delete again*) is
  `components/WorkspaceAction.vue`, chosen by `lib/workspaceCard.ts`.
- `lib/workspaceCard.ts` is §6.1's table as a pure function of both halves: `supervisorHalf()`
  reads the reducer's supervisor entity (`supervisor.state` events, the views' `supervisor`), never
  `workspace.state`, and `cardStatus(w, fleet)` takes the fleet login (`stream.entities.identity`)
  for §6.6's override: a signed-out fleet leaves the card *Running* with no session action, and
  *"Waiting on Claude sign-in."* is `WorkspaceIdentityNote`'s alone, never also the card's line. A
  view with no `supervisor` field (a server older than Phase 5) keeps the workspace half. The
  environment link is built from an `env_…` id, never taken from the wire.
- Delete's confirm compares the typed name exactly and sends it as typed; a stuck delete's resume
  asks for nothing. `workspace.action` events are reduced into `Workspace.action`, and `liveAction`
  says whether a stop or delete is still running; a failed stop and a stuck delete are ended by the
  server's annotation, and `stopFailed`/`deleteStuck` read them — from the list alone too, via
  `lastAction`.
- The occupied count is **counted, not stored** (`lib/capacity.ts`, against the cap
  `GET /api/workspaces` carries), and `capacity.spec.ts` reads `Occupying` out of
  `internal/workspace/state.go` so the client's rule cannot drift. At the cap an action that would
  take a slot is replaced by `MakeRoom`, never disabled, and Running sorts stoppable cards first.
- A stopped workspace waiting on a host-access request (design §6) shows
  `components/HostAccessApproval.vue` where its action would be — the added, changed and removed
  settings as JSON text, the warning (root on the host when `privileged` is asked), *Approve and
  continue* (sends the request's own hash) and *Cancel* — on the card, the catalog row and the
  detail view alike; the request is the reducer's `approval`, written only by `workspace.state`
  events and the views.
- `components/ClaudeLogin.vue` is the login handshake (frontend §6.2) in Settings' Claude section,
  its state the reducer's `login` field (the GET's `login` and `auth.login`); on Settings the fleet
  banner drops its link, so its button is the one *Sign in to Claude*. The code is one component
  ref in an `<input>` in no `<form>`, shape-checked by `lib/login.ts` (whose spec reads the Go
  rule), cleared before the request, and swept by a canary spec.

## Ports: `src/components/PortsPanel.vue`, `src/stores/ports.ts`

The ports panel (frontend §6.3; port forwarding §13 step 4), on the workspace's page under its card.

- Port entities (`entities.ports`, by row id) are written only by `GET …/ports?hidden=true` (the
  `ports` snapshot) and the `port.*` events, which carry the whole row; each is versioned by event
  id. `port.retired` tombstones the id (`portsRetired`), so neither a late event nor an older list
  revives it, and **only the list drops a live row**, as only the workspace list drops a workspace.
  `workspace.gone` takes a workspace's rows.
- Every switch is an `ActionButton` keyed per row (`portEnableKey` and friends), ended by its event
  or a later snapshot showing it over — one predicate, `OVER_PORT`, for both — and never by the
  `202`. An add is settled by `port.added` for that number on that workspace.
- The link is the row's `url`, taken only when it is `https://`, opened in a new tab with
  `rel="noopener noreferrer"`. With no preview domain (`previews: false`) the panel offers no switch.
- **Check the port** is a GET whose answer is the proxy's own sentence; it is the panel's read,
  never entity state. "Show hidden" is a `localStorage` preference.

## Secrets UI: `src/views/secrets/`, `src/lib/secretRules.ts`

One lazy chunk: the list, the write-only form (`reach` labelled with the literal question), grants
with the `all_repos` confirm, the two-kinds rotate result, delete.

- Secret entities are written only by `GET /api/secrets` and `secret.*` events. The `PUT`'s 200
  body is used for its `stale` lists and never applied.
- The fleet banner's undeliverable fault is the same kind of entity — from the list's
  `undeliverable` and the two delivery events, versioned — and `FleetBanner.vue` loads the list
  itself, since it is on every screen.
- The form's edit mode sends **no `value` key** when the value field is empty (keep the stored
  one), never `""`, and so never asks for the current value to change the reach.
- The value lives in one component `ref`, in a `<textarea>` (an `<input>` strips a pasted newline)
  inside no `<form>`, and a canary spec proves it is nowhere after submit.
- `secretRules.ts` mirrors `internal/secrets/validate.go`, and its spec reads that file, so
  **change a reserved name or a limit in Go and that spec fails until the copy matches**.
- **New secret never replaces**: it refuses a name the loaded list holds as it is typed, offers
  *Edit NAME instead*, and sends the `PUT` with `If-None-Match: *`, so a name another device stored
  meanwhile is the server's `412 secret_exists`, on the name field. The edit form sends no such
  header.

## The mock backend: `src/mocks/backend.ts`

- `supervisor: true` plays the session server (`dev:mock` sets it; specs default to false).
- It serves the workspace routes with the cap and the duplicate check; `scriptMode: 'manual'`
  holds a create's events for a spec to play with `playScript`. `schedule` ends every job's script
  with its `workspace.job`, as the server's `launch` does (a script's `outcome` and `endAt` say
  how and where), and the job a delete cuts off ends `cancelled` after the move to `deleting`,
  before the delete's first sub-step.
- `loginMode: 'manual'` holds each login phase for `loginReady`/`loginVerdict`.
- It serves the port routes with the server's refusals and one `port.*` event per mutation;
  `seedPorts` (dev:mock's) declares a listening Vite port and a silent one on the running sample,
  `listening` is what a probe finds answering, and `previewDomain: null` is previews off.
- It checks identity as the watch does (`startIdentityCheck`/`finishIdentityCheck`/
  `intervalIdentityCheck`, `identityCheckMode: 'manual'`): silent for an unchanged interval check,
  `auth.identity_checked` for an unchanged requested one, a press during a check answered by the check queued after it, and
  `identityWatchStopped` refusing a check `503` as the route does after shutdown. **A mock that
  announces every check passes specs a real server fails** — keep the mock as strict as the
  server.
