// The reducer (frontend §4.1, §10): the one function that writes entity state.
//
// It is pure — (entities, action) → entities, never mutating its input — so
// every case it handles is a plain Vitest test over a recorded event sequence
// (reducer.spec.ts). Three kinds of input reach it:
//
//   event     an unnamed frame off GET /api/events
//   resync    the one named frame: replay could not close the gap
//   snapshot  a GET /api/repos body, with the stream position it was asked at
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
// entity: the activity feed is where those belong, and an unknown kind must
// never be an error — the server names nothing precisely so a kind the client
// does not know reaches this branch rather than vanishing (events_routes.go).

import { WORKSPACE_STATES, type CatalogView, type InstallationView, type RepoView, type StreamEvent, type WorkspaceState } from '../api/types'

export interface StepInfo {
  name: string
  status: 'started' | 'done' | 'failed'
  detail: string | null
}

export interface Workspace {
  id: string
  /** Null when only an event without one has been seen (a stub). */
  repositoryId: number | null
  branch: string | null
  /** Null for a stub: an event named this workspace before anything said its state. */
  state: WorkspaceState | null
  detail: string | null
  step: StepInfo | null
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
   * snapshot taken at or after this id clears it — one already in flight
   * when the resync arrived predates the gap and does not.
   */
  staleSince: number | null
  workspaces: Record<string, Workspace>
  /**
   * Deleted workspaces, by the id of their `workspace.gone`. Ids are ULIDs and
   * never reused, so a late event or an older snapshot naming one is ignored
   * rather than resurrecting it.
   */
  gone: Record<string, number>
  repos: Record<number, Repo>
  /** The server's order (newest push first); `repos` is keyed, this is the list. */
  repoOrder: number[]
  installations: Record<number, InstallationView>
  /** Null until a snapshot has been applied: "not loaded", distinct from "empty". */
  catalogRefreshedAt: string | null
  catalogLoaded: boolean
  lastRefresh: RefreshOutcome | null
}

export type Action =
  | { type: 'event'; event: StreamEvent }
  | { type: 'resync'; id: number }
  | { type: 'snapshot'; at: number; view: CatalogView }

export function emptyEntities(): Entities {
  return {
    lastEventId: 0,
    staleSince: null,
    workspaces: {},
    gone: {},
    repos: {},
    repoOrder: [],
    installations: {},
    catalogRefreshedAt: null,
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
  }
}

/** Folds a recorded sequence; the specs' and the store's shared entry point. */
export function reduceAll(start: Entities, actions: Action[]): Entities {
  return actions.reduce(reduce, start)
}

function isState(v: unknown): v is WorkspaceState {
  return typeof v === 'string' && (WORKSPACE_STATES as readonly string[]).includes(v)
}

function str(v: unknown): string | null {
  return typeof v === 'string' && v !== '' ? v : null
}

function num(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

function stub(id: string): Workspace {
  return {
    id, repositoryId: null, branch: null, state: null, detail: null, step: null,
    adopted: false, stateAt: 0, stepAt: 0,
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

  if (!ev.kind.startsWith('workspace.')) return base
  const wsId = str(ev.workspace_id)
  if (wsId === null) return base
  if (prev.gone[wsId] !== undefined) return base // deleted; nothing brings it back

  if (ev.kind === 'workspace.gone') {
    const workspaces = { ...base.workspaces }
    delete workspaces[wsId]
    return { ...base, workspaces, gone: { ...base.gone, [wsId]: ev.id } }
  }

  // Every other workspace.* kind names a workspace that exists. One this
  // client has never heard of becomes a stub, so the event is not lost and
  // the store can see that a refetch is owed (the stub's state is null).
  const known = base.workspaces[wsId]
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
      // A new run through the steps begins at pending (a create) or at a
      // rebuild's move out of a resting state; the previous run's last step
      // must not be shown as this run's progress.
      if (data.state === 'pending' && next.stepAt < ev.id) next = { ...next, step: null, stepAt: ev.id }
      break
    }
    case 'workspace.step': {
      const name = str(data.step)
      const status = data.status
      if (name === null || (status !== 'started' && status !== 'done' && status !== 'failed')) break
      if (ev.id <= cur.stepAt) break
      next = { ...cur, step: { name, status, detail: str(data.detail) }, stepAt: ev.id }
      break
    }
    case 'workspace.adopted':
      // A known row matched to its container at boot; the state is unchanged.
      next = cur.adopted ? cur : { ...cur, adopted: true }
      break
    default:
      // A workspace kind from a later phase: create the stub if it was
      // unknown, change nothing else.
      break
  }

  if (next === cur && known !== undefined) return base
  return { ...base, workspaces: { ...base.workspaces, [wsId]: next } }
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
 * a straggling event from before the snapshot cannot undo it. A workspace the
 * snapshot does not mention, and that nothing after `at` has touched, is gone
 * from the server's view and is dropped — which is also how a stub the
 * refetch could not explain stops asking for refetches.
 *
 * Repositories carry no event ids; the snapshot is their only source, so it
 * replaces them outright.
 */
function applySnapshot(prev: Entities, at: number, view: CatalogView): Entities {
  const repos: Record<number, Repo> = {}
  const repoOrder: number[] = []
  const installations: Record<number, InstallationView> = {}
  for (const i of view.installations) installations[i.id] = i

  const seen = new Set<string>()
  const workspaces: Record<string, Workspace> = {}
  for (const r of view.repos) {
    repos[r.id] = toRepo(r)
    repoOrder.push(r.id)
    const w = r.workspace
    if (w === null || !isState(w.state)) continue
    const gone = prev.gone[w.id]
    if (gone !== undefined) continue // the snapshot predates the delete, or races it
    seen.add(w.id)
    const cur = prev.workspaces[w.id]
    if (cur !== undefined && cur.stateAt > at) {
      workspaces[w.id] = cur.repositoryId === null ? { ...cur, repositoryId: r.id } : cur
    } else if (cur !== undefined && cur.state === w.state) {
      workspaces[w.id] = { ...cur, repositoryId: r.id, stateAt: Math.max(cur.stateAt, at) }
    } else {
      workspaces[w.id] = {
        ...(cur ?? stub(w.id)),
        repositoryId: r.id, state: w.state, detail: null, stateAt: at,
      }
    }
  }
  for (const [id, w] of Object.entries(prev.workspaces)) {
    if (seen.has(id)) continue
    if (w.stateAt > at || w.stepAt > at) workspaces[id] = w
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
    catalogLoaded: true,
  }
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
