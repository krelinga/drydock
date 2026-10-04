// The reducer (frontend §4.1, §10): the one function that writes entity state.
//
// It is pure — (entities, action) → entities, never mutating its input — so
// every case it handles is a plain Vitest test over a recorded event sequence
// (reducer.spec.ts). Five kinds of input reach it:
//
//   event       an unnamed frame off GET /api/events
//   resync      the one named frame: replay could not close the gap
//   snapshot    a GET /api/repos body, with the stream position it was asked at
//   workspaces  a GET /api/workspaces body, likewise
//   workspace   a GET /api/workspaces/:id body — one workspace and its recent
//               events — likewise
//
// Of the three snapshots only `workspaces` is the authority on which
// workspaces exist: it lists every row, where the catalog joins only each
// repository's newest (and not a `deleting` one) and the detail names one. So
// it alone drops a workspace it does not mention.
//
// Ordering is by event id, not arrival. Every field that events write carries
// the id of the event that last wrote it, and an event at or below that id is
// a no-op. That one rule is what makes a replayed gap idempotent (the browser
// re-sends Last-Event-ID; the server may overlap what we already applied) and
// what keeps a late, out-of-order event from rolling a state backwards. Ids are
// assigned by the server under the same lock that publishes them
// (internal/events), so a higher id is always the later fact.
//
// Event data shapes, from the Emit calls that write them:
//
//   workspace.state    {state, from, detail?}                  workspace.Move
//                      {state, repository_id, branch}          workspace.Create
//                      {state, adopted, repository_id, branch, detail?}  Adopt
//   workspace.step     {step, status: started|done|failed, detail?}
//   workspace.gone     {}
//   workspace.adopted  {container_id}       a known row matched to a container
//   repo.refreshed     {count, added, removed}
//   repo.refresh_failed {}                  the reason is in `message`
//
// Every other kind (token.*, container.unclaimed, system.reconcile, and
// whatever a later phase adds) advances the stream position and changes no
// workspace or repository: if it names a workspace it joins that workspace's
// feed, and that is all. An unknown kind must never be an error — the server
// names nothing precisely so a kind the client does not know reaches this
// branch rather than vanishing (events_routes.go).

import {
  WORKSPACE_STATES, WORKSPACE_STEPS, type CatalogView, type InstallationView, type RepoView, type StepStatus,
  type StreamEvent, type WorkspaceDetail, type WorkspaceList, type WorkspaceState, type WorkspaceView,
} from '../api/types'

export interface StepInfo {
  name: string
  status: StepStatus
  detail: string | null
}

/** One step's latest status, versioned on its own. */
export interface StepRecord {
  status: StepStatus
  detail: string | null
  /** When the server says it happened. */
  at: string
  /** The id of the event (or snapshot position) that wrote it. */
  eventId: number
}

/** How many events a workspace's feed keeps: what GET /api/workspaces/:id returns. */
export const FEED_LIMIT = 50

export interface Workspace {
  id: string
  /** Null when only an event without one has been seen (a stub). */
  repositoryId: number | null
  /** From a snapshot: the workspace list's own, or the catalog row's. */
  fullName: string | null
  branch: string | null
  /** Null for a stub: an event named this workspace before anything said its state. */
  state: WorkspaceState | null
  /** `state_detail`: the server's sentence about the state. Shown, never parsed. */
  detail: string | null
  /** The most recent step event: what the card's progress line names. */
  step: StepInfo | null
  /** Each step's latest status, by name: the detail view's timeline. */
  steps: Record<string, StepRecord>
  containerId: string | null
  createdAt: string | null
  adopted: boolean
  /** The id of the event (or snapshot position) that last wrote `state`. */
  stateAt: number
  /** The id of the event that last wrote `step`. */
  stepAt: number
}

export interface Repo {
  id: number
  installationId: number
  fullName: string
  defaultBranch: string
  private: boolean
  archived: boolean
  hasDevcontainer: boolean | null
  pushedAt: string | null
  removed: boolean
}

export interface RefreshOutcome {
  ok: boolean
  at: string
  count: number | null
  added: number | null
  removed: number | null
  /** The event's human sentence. Shown, never parsed. */
  message: string
  /** The event id, so an older outcome cannot replace a newer one. */
  eventId: number
}

export interface Entities {
  /** The highest event id applied. What the next snapshot is "as of". */
  lastEventId: number
  /**
   * The id of a `resync` no snapshot has yet caught up with, or null. While it
   * is set the entities may be wrong: render them, but label them. Only a
   * catalog or workspace-list snapshot taken at or after this id clears it —
   * one already in flight when the resync arrived predates the gap and does
   * not, and a single workspace's detail says nothing about the rest.
   */
  staleSince: number | null
  workspaces: Record<string, Workspace>
  /**
   * Deleted workspaces, by the id of their `workspace.gone`. Ids are ULIDs and
   * never reused, so a late event or an older snapshot naming one is ignored
   * rather than resurrecting it.
   */
  gone: Record<string, number>
  /**
   * Each workspace's recent events, newest first, at most FEED_LIMIT. Fed by
   * every event naming a workspace — token.issued included — and by the
   * detail snapshot, merged by id so the two never double an entry.
   */
  feeds: Record<string, StreamEvent[]>
  repos: Record<number, Repo>
  /** The server's order (newest push first); `repos` is keyed, this is the list. */
  repoOrder: number[]
  installations: Record<number, InstallationView>
  /** Null until a snapshot has been applied: "not loaded", distinct from "empty". */
  catalogRefreshedAt: string | null
  catalogRefreshError: { at: string; message: string } | null
  catalogLoaded: boolean
  lastRefresh: RefreshOutcome | null
}

export type Action =
  | { type: 'event'; event: StreamEvent }
  | { type: 'resync'; id: number }
  | { type: 'snapshot'; at: number; view: CatalogView }
  | { type: 'workspaces'; at: number; view: WorkspaceList }
  | { type: 'workspace'; at: number; view: WorkspaceDetail }

export function emptyEntities(): Entities {
  return {
    lastEventId: 0,
    staleSince: null,
    workspaces: {},
    gone: {},
    feeds: {},
    repos: {},
    repoOrder: [],
    installations: {},
    catalogRefreshedAt: null,
    catalogRefreshError: null,
    catalogLoaded: false,
    lastRefresh: null,
  }
}

export function reduce(prev: Entities, action: Action): Entities {
  switch (action.type) {
    case 'event':
      return applyEvent(prev, action.event)
    case 'resync':
      // The entities cannot be repaired from here — the events that would
      // repair them are gone. Advance to where the server says the stream
      // now is, keep rendering what we have, and say it may be stale until
      // the refetch the store issues lands as a snapshot.
      return {
        ...prev,
        lastEventId: Math.max(prev.lastEventId, action.id),
        staleSince: Math.max(prev.staleSince ?? 0, action.id),
      }
    case 'snapshot':
      return applySnapshot(prev, action.at, action.view)
    case 'workspaces':
      return applyWorkspaceList(prev, action.at, action.view)
    case 'workspace':
      return applyWorkspaceDetail(prev, action.at, action.view)
  }
}

/** Folds a recorded sequence; the specs' and the store's shared entry point. */
export function reduceAll(start: Entities, actions: Action[]): Entities {
  return actions.reduce(reduce, start)
}

function isState(v: unknown): v is WorkspaceState {
  return typeof v === 'string' && (WORKSPACE_STATES as readonly string[]).includes(v)
}

function isStepStatus(v: unknown): v is StepStatus {
  return v === 'started' || v === 'done' || v === 'failed'
}

function str(v: unknown): string | null {
  return typeof v === 'string' && v !== '' ? v : null
}

function num(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

function stub(id: string): Workspace {
  return {
    id, repositoryId: null, fullName: null, branch: null, state: null, detail: null, step: null,
    steps: {}, containerId: null, createdAt: null, adopted: false, stateAt: 0, stepAt: 0,
  }
}

function applyEvent(prev: Entities, ev: StreamEvent): Entities {
  if (!Number.isInteger(ev.id) || ev.id <= 0) return prev // not an id the server issues
  const lastEventId = Math.max(prev.lastEventId, ev.id)
  const base = lastEventId === prev.lastEventId ? prev : { ...prev, lastEventId }
  const data = ev.data ?? {}

  if (ev.kind === 'repo.refreshed' || ev.kind === 'repo.refresh_failed') {
    if (prev.lastRefresh !== null && prev.lastRefresh.eventId >= ev.id) return base
    const ok = ev.kind === 'repo.refreshed'
    return {
      ...base,
      lastRefresh: {
        ok, at: ev.at, message: ev.message, eventId: ev.id,
        count: ok ? num(data.count) : null,
        added: ok ? num(data.added) : null,
        removed: ok ? num(data.removed) : null,
      },
    }
  }

  const wsId = str(ev.workspace_id)
  if (wsId === null) return base
  if (prev.gone[wsId] !== undefined) return base // deleted; nothing brings it back

  if (ev.kind === 'workspace.gone') {
    const workspaces = { ...base.workspaces }
    delete workspaces[wsId]
    const feeds = { ...base.feeds }
    delete feeds[wsId]
    return { ...base, workspaces, feeds, gone: { ...base.gone, [wsId]: ev.id } }
  }

  // Every event naming a workspace joins its feed, whatever its kind.
  const feed = mergeFeed(base.feeds[wsId], [ev])
  const fed = feed === base.feeds[wsId] ? base : { ...base, feeds: { ...base.feeds, [wsId]: feed } }

  if (!ev.kind.startsWith('workspace.')) return fed

  // Every other workspace.* kind names a workspace that exists. One this
  // client has never heard of becomes a stub, so the event is not lost and
  // the store can see that a refetch is owed (the stub's state is null).
  const known = fed.workspaces[wsId]
  const cur = known ?? stub(wsId)
  let next: Workspace = cur

  switch (ev.kind) {
    case 'workspace.state': {
      if (!isState(data.state) || ev.id <= cur.stateAt) break
      next = {
        ...cur,
        state: data.state,
        detail: str(data.detail),
        stateAt: ev.id,
        repositoryId: num(data.repository_id) ?? cur.repositoryId,
        branch: str(data.branch) ?? cur.branch,
        adopted: data.adopted === true ? true : cur.adopted,
      }
      // A new run through the steps begins at pending (a create); nothing
      // written before it belongs to this workspace's timeline. Start moves
      // stopped or failed to building instead, and each step it reruns
      // overwrites its own row, so the timeline is "latest per step" — the
      // same thing the server's `steps` reports.
      if (data.state === 'pending') {
        if (next.stepAt < ev.id) next = { ...next, step: null, stepAt: ev.id }
        const kept = Object.entries(next.steps).filter(([, r]) => r.eventId > ev.id)
        if (kept.length !== Object.keys(next.steps).length) next = { ...next, steps: Object.fromEntries(kept) }
      }
      break
    }
    case 'workspace.step': {
      const name = str(data.step)
      const status = data.status
      if (name === null || !isStepStatus(status)) break
      const detail = str(data.detail)
      // The timeline and the progress line are versioned apart: a late
      // event for one step still lands in its own row after a newer step has
      // moved the progress line on.
      const old = cur.steps[name]
      if (old === undefined || ev.id > old.eventId) {
        next = { ...next, steps: { ...cur.steps, [name]: { status, detail, at: ev.at, eventId: ev.id } } }
      }
      if (ev.id > cur.stepAt) next = { ...next, step: { name, status, detail }, stepAt: ev.id }
      break
    }
    case 'workspace.adopted': {
      // A known row matched to its container at boot; the state is unchanged.
      const containerId = str(data.container_id) ?? cur.containerId
      if (!cur.adopted || containerId !== cur.containerId) next = { ...cur, adopted: true, containerId }
      break
    }
    default:
      // A workspace kind from a later phase: create the stub if it was
      // unknown, change nothing else.
      break
  }

  if (next === cur && known !== undefined) return fed
  return { ...fed, workspaces: { ...fed.workspaces, [wsId]: next } }
}

/** Merges events into a feed: by id, newest first, capped. Returns `feed` itself when nothing changed. */
function mergeFeed(feed: StreamEvent[] | undefined, add: StreamEvent[]): StreamEvent[] {
  const cur = feed ?? []
  const have = new Set(cur.map((e) => e.id))
  const fresh = add.filter((e) => Number.isInteger(e.id) && e.id > 0 && !have.has(e.id))
  if (fresh.length === 0) return feed ?? cur
  const out = [...cur, ...fresh].sort((a, b) => b.id - a.id).slice(0, FEED_LIMIT)
  // Everything new fell off the end: nothing visible changed.
  if (feed !== undefined && out.length === cur.length && out.every((e, i) => e === cur[i])) return feed
  return out
}

function toRepo(r: RepoView): Repo {
  return {
    id: r.id,
    installationId: r.installation_id,
    fullName: r.full_name,
    defaultBranch: r.default_branch,
    private: r.private,
    archived: r.archived,
    hasDevcontainer: r.has_devcontainer,
    pushedAt: r.pushed_at,
    removed: r.removed,
  }
}

/**
 * Merges a GET /api/repos body taken when the stream stood at `at`.
 *
 * The body reflects at least every event up to `at` (they were committed
 * before the request was made) and maybe some after. So for each workspace:
 * if an event newer than `at` has already been applied, ours is the fresher
 * fact and is kept; otherwise the snapshot's state wins, at version `at`, so
 * a straggling event from before the snapshot cannot undo it.
 *
 * A workspace the catalog does not mention is kept. The catalog joins only
 * each repository's newest workspace, and never a `deleting` one, so its
 * silence says nothing about whether an older one exists; GET
 * /api/workspaces is what can say that (applyWorkspaceList).
 *
 * Repositories carry no event ids; the snapshot is their only source, so it
 * replaces them outright.
 */
function applySnapshot(prev: Entities, at: number, view: CatalogView): Entities {
  const repos: Record<number, Repo> = {}
  const repoOrder: number[] = []
  const installations: Record<number, InstallationView> = {}
  for (const i of view.installations) installations[i.id] = i

  const workspaces: Record<string, Workspace> = { ...prev.workspaces }
  for (const r of view.repos) {
    repos[r.id] = toRepo(r)
    repoOrder.push(r.id)
    const w = r.workspace
    if (w === null || !isState(w.state)) continue
    const gone = prev.gone[w.id]
    if (gone !== undefined) continue // the snapshot predates the delete, or races it
    const cur = prev.workspaces[w.id]
    if (cur !== undefined && cur.stateAt > at) {
      workspaces[w.id] = cur.repositoryId === null ? { ...cur, repositoryId: r.id, fullName: r.full_name } : cur
    } else if (cur !== undefined && cur.state === w.state) {
      workspaces[w.id] = { ...cur, repositoryId: r.id, fullName: cur.fullName ?? r.full_name, stateAt: Math.max(cur.stateAt, at) }
    } else {
      workspaces[w.id] = {
        ...(cur ?? stub(w.id)),
        repositoryId: r.id, fullName: r.full_name, state: w.state, detail: null, stateAt: at,
      }
    }
  }

  return {
    ...prev,
    lastEventId: Math.max(prev.lastEventId, at),
    staleSince: prev.staleSince !== null && at < prev.staleSince ? prev.staleSince : null,
    workspaces,
    repos,
    repoOrder,
    installations,
    catalogRefreshedAt: view.refreshed_at,
    catalogRefreshError: view.last_refresh_error,
    catalogLoaded: true,
  }
}

/**
 * One workspace from a GET /api/workspaces(/:id) body taken at `at`, merged
 * over what the entities already hold. The rule is applySnapshot's: a fact an
 * event newer than `at` wrote is kept, anything else takes the body's,
 * versioned at `at`. State and its detail move together; each step is
 * versioned on its own, so a step event that beat the body keeps its row.
 */
function mergeView(cur: Workspace | undefined, at: number, v: WorkspaceView): Workspace {
  const base = cur ?? stub(v.id)
  const steps: Record<string, StepRecord> = { ...base.steps }
  for (const [name, sv] of Object.entries(v.steps ?? {})) {
    if (!isStepStatus(sv.status)) continue
    const old = steps[name]
    if (old !== undefined && old.eventId > at) continue
    steps[name] = { status: sv.status, detail: str(sv.detail), at: sv.at, eventId: at }
  }
  const eventsWin = base.stateAt > at
  return {
    ...base,
    repositoryId: v.repository_id,
    fullName: v.full_name,
    branch: v.branch,
    state: eventsWin ? base.state : v.state,
    detail: eventsWin ? base.detail : v.state_detail,
    stateAt: eventsWin ? base.stateAt : at,
    containerId: v.container_id ?? (eventsWin ? base.containerId : null),
    createdAt: v.created_at,
    steps,
  }
}

function isView(v: WorkspaceView): boolean {
  return typeof v.id === 'string' && v.id !== '' && isState(v.state)
}

/**
 * Merges a GET /api/workspaces body: every workspace with a row. Unlike the
 * catalog it is the authority on existence — a workspace it does not list,
 * and that no event after `at` has touched, is gone from the server and is
 * dropped. That is also how a stub the refetch could not explain stops
 * asking for refetches.
 */
function applyWorkspaceList(prev: Entities, at: number, view: WorkspaceList): Entities {
  const workspaces: Record<string, Workspace> = {}
  for (const v of view.workspaces) {
    if (!isView(v) || prev.gone[v.id] !== undefined) continue
    workspaces[v.id] = mergeView(prev.workspaces[v.id], at, v)
  }
  for (const [id, w] of Object.entries(prev.workspaces)) {
    if (workspaces[id] !== undefined) continue
    if (w.stateAt > at || w.stepAt > at) workspaces[id] = w
  }
  const feeds: Record<string, StreamEvent[]> = {}
  for (const [id, f] of Object.entries(prev.feeds)) {
    if (workspaces[id] !== undefined || f.some((e) => e.id > at)) feeds[id] = f
  }
  return {
    ...prev,
    lastEventId: Math.max(prev.lastEventId, at),
    staleSince: prev.staleSince !== null && at < prev.staleSince ? prev.staleSince : null,
    workspaces,
    feeds,
  }
}

/**
 * Merges a GET /api/workspaces/:id body: the one workspace, and its recent
 * events into its feed. It names one workspace, so it drops none — and it
 * does not clear a resync, because the rest of the entities are no fresher
 * for it.
 */
function applyWorkspaceDetail(prev: Entities, at: number, view: WorkspaceDetail): Entities {
  if (!isView(view) || prev.gone[view.id] !== undefined) {
    return at > prev.lastEventId ? { ...prev, lastEventId: at } : prev
  }
  const events = (view.events ?? []).filter((e) => str(e.workspace_id) === view.id)
  return {
    ...prev,
    lastEventId: Math.max(prev.lastEventId, at),
    workspaces: { ...prev.workspaces, [view.id]: mergeView(prev.workspaces[view.id], at, view) },
    feeds: { ...prev.feeds, [view.id]: mergeFeed(prev.feeds[view.id], events) },
  }
}

const stepOrder = (n: string) => (WORKSPACE_STEPS as readonly string[]).indexOf(n)

/**
 * The step the card names: the most recent step event, or — when only a
 * snapshot has been seen — the latest-written row of the timeline, ties
 * broken by run order.
 */
export function currentStep(w: Workspace): StepInfo | null {
  if (w.step !== null) return w.step
  let best: [string, StepRecord] | null = null
  for (const entry of Object.entries(w.steps)) {
    if (best === null || entry[1].eventId > best[1].eventId
      || (entry[1].eventId === best[1].eventId && stepOrder(entry[0]) > stepOrder(best[0]))) best = entry
  }
  return best === null ? null : { name: best[0], status: best[1].status, detail: best[1].detail }
}

/** The step a failed workspace failed at, or null when nothing says. */
export function failedStep(w: Workspace): string | null {
  if (w.step?.status === 'failed') return w.step.name
  let best: [string, StepRecord] | null = null
  for (const entry of Object.entries(w.steps)) {
    if (entry[1].status !== 'failed') continue
    if (best === null || entry[1].eventId > best[1].eventId) best = entry
  }
  return best?.[0] ?? null
}

/** The newest workspace holding a repository: the join the home list shows. */
export function workspaceForRepo(e: Entities, repoId: number): Workspace | null {
  let best: Workspace | null = null
  for (const w of Object.values(e.workspaces)) {
    if (w.repositoryId !== repoId || w.state === null) continue
    // ULIDs sort by creation time, which is the server's own "newest" (view.go).
    if (best === null || w.id > best.id) best = w
  }
  return best
}

/** Whether the store owes a refetch: something named a workspace it cannot place. */
export function hasStubs(e: Entities): boolean {
  return Object.values(e.workspaces).some((w) => w.state === null || w.repositoryId === null)
}
