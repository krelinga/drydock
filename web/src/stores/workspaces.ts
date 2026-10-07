// The workspaces store (frontend §4.1): the fetches that feed workspace
// entities, and the mutations — create and start (Phase 2), stop, rebuild
// and delete (Phase 6).
//
// Like the catalog it writes no entity itself. GET /api/workspaces and GET
// /api/workspaces/:id are handed to the stream store as snapshots tagged with
// the stream position they were asked at, and the reducer merges them. What
// this store owns is each request's own lifecycle — loading, not found,
// failed — which is not entity state.
//
// Every mutation is §4.2 to the letter: mark in flight, send, discard the
// 202, and let an event clear the mark. They resolve when the server accepts
// and throw (clearing the mark) only when it refuses. What clears each mark
// is the `settles*` function beside its key — the event that means the
// action is over, not merely begun.

import { defineStore } from 'pinia'
import * as api from '../api/client'
import { WORKSPACE_STATES, type StreamEvent, type WorkspaceDetail, type WorkspaceList, type WorkspaceState } from '../api/types'
import { deleteStuck, repoHeld, stopFailed, type Entities } from './reducer'
import { useStreamStore } from './stream'

export type LoadStatus = 'idle' | 'loading' | 'ready' | 'not_found' | 'error'

/** The in-flight key for cloning a repository (§4.2 step 1). */
export const cloneKey = (repoId: number) => `repo:${repoId}:clone`
/** The in-flight key for starting a workspace. */
export const startKey = (id: string) => `workspace:${id}:start`
/** The in-flight key for stopping a workspace. */
export const stopKey = (id: string) => `workspace:${id}:stop`
/** The in-flight key for rebuilding a workspace. */
export const rebuildKey = (id: string) => `workspace:${id}:rebuild`
/** The in-flight key for deleting a workspace, a resumed delete included. */
export const deleteKey = (id: string) => `workspace:${id}:delete`
/** The in-flight key for starting or restarting a workspace's session server. */
export const sessionKey = (id: string) => `workspace:${id}:session`

const MOVING_STATES: ReadonlySet<string> = new Set(['pending', 'cloning', 'building'])

/**
 * What one input says about how a workspace's action ended — from a stream
 * event (`eventOutcome`) or from the entities after a snapshot
 * (`entityOutcome`). Each action's "is it over?" is one predicate over this
 * (`OVER`), shared by its `settles*` function and its snapshot resolver, so
 * the event path and the snapshot path cannot disagree about what ends it.
 */
export interface Outcome {
  /** The workspace no longer exists. */
  gone: boolean
  /** The state the event moves it to, or the state the entities hold; null when the input says none. */
  state: WorkspaceState | null
  /** A delete stopped part-way and is annotated as such (`deleteStuck`). */
  stuckDelete: boolean
  /** A stop's sub-step failed, leaving the workspace running (`stopFailed`). */
  failedStop: boolean
}

/** What the event says about workspace `id`'s action, or null when it says nothing. */
export function eventOutcome(id: string, ev: StreamEvent): Outcome | null {
  if (ev.workspace_id !== id) return null
  const none: Outcome = { gone: false, state: null, stuckDelete: false, failedStop: false }
  const d = ev.data ?? {}
  switch (ev.kind) {
    case 'workspace.gone':
      return { ...none, gone: true }
    case 'workspace.state': {
      const state = (WORKSPACE_STATES as readonly unknown[]).includes(d.state) ? d.state as WorkspaceState : null
      // Annotate, which writes a state event that moves nothing: a stuck
      // delete is deleting to deleting, and a failed stop running to
      // running, each with the sentence. The server writes nothing else of
      // that shape (an orphan's adoption carries no `from`), and both are
      // what the row keeps, so a snapshot reads the same end (`deleteStuck`,
      // `stopFailed`). Not the failed sub-step before it: a body read
      // between the two cannot yet tell a failed stop from one under way.
      const annotated = d.from === state && typeof d.detail === 'string' && d.detail !== ''
      return { ...none, state, stuckDelete: annotated && state === 'deleting', failedStop: annotated && state === 'running' }
    }
    default:
      return null
  }
}

/** What the entities say about workspace `id`'s action: as of the last snapshot, newer events included. */
export function entityOutcome(e: Entities, id: string): Outcome {
  const w = e.workspaces[id]
  // Only the workspace list, or `workspace.gone`, removes a workspace from
  // the entities (reducer.ts), so its absence is the server's word.
  if (w === undefined || e.gone[id] !== undefined) return { gone: true, state: null, stuckDelete: false, failedStop: false }
  return { gone: false, state: w.state, stuckDelete: deleteStuck(w), failedStop: stopFailed(w) }
}

/**
 * Each action's end, over an Outcome. Not the move the server commits before
 * answering 202 — the move into `building` or `deleting`, the cleared
 * annotation — which is its receipt, not its end (§4.2).
 */
export const OVER = {
  /** A stop: `stopped`, a sub-step's failure (the workspace stays running), or a delete overtaking it. */
  stop: (o: Outcome) => o.gone || o.state === 'stopped' || o.state === 'deleting' || o.failedStop,
  /**
   * A start or a rebuild: leaving its build — for `running` or `failed`, or a
   * delete. Not the move *into* `building` or `cloning`, which the server
   * commits before answering, and not a step: a build is minutes of steps,
   * and the mark is what keeps a second tap from starting another.
   */
  build: (o: Outcome) => o.gone || (o.state !== null && !MOVING_STATES.has(o.state)),
  /** A delete: `workspace.gone`, or the annotation of one that stuck, after which Delete is the button again. */
  delete: (o: Outcome) => o.gone || o.stuckDelete,
  /** A session server (re)start, as far as the workspace can say: it left `running`, or went. */
  session: (o: Outcome) => o.gone || (o.state !== null && o.state !== 'running'),
} as const

function settlesBy(id: string, over: (o: Outcome) => boolean): (ev: StreamEvent) => boolean {
  return (ev) => {
    const o = eventOutcome(id, ev)
    return o !== null && over(o)
  }
}

function overIn(id: string, over: (o: Outcome) => boolean): (e: Entities) => boolean {
  return (e) => over(entityOutcome(e, id))
}

/**
 * What settles a clone: the create's own `workspace.state`, which carries the
 * repository id. The client has no workspace id until the 202's body names
 * one, and reading that body is exactly what §4.2 step 3 forbids — so the
 * clone is matched by the one id it did have. A create from another device
 * settles it too, which is right: the server answers ours `in_progress`.
 */
export function settlesClone(repoId: number): (ev: StreamEvent) => boolean {
  return (ev) => ev.kind === 'workspace.state' && ev.data?.repository_id === repoId
}

/** The same, asked of the entities: any workspace holds the repository (the create's row exists). */
export function cloneOverIn(repoId: number): (e: Entities) => boolean {
  return (e) => repoHeld(e, repoId)
}

/**
 * What settles a start: the workspace leaving its build, as a rebuild does —
 * never the move into `building` or `cloning`, which `restart()` commits
 * before it answers 202 (frontend §4.2: the event that ends the action).
 */
export function settlesStart(id: string): (ev: StreamEvent) => boolean {
  return settlesBy(id, OVER.build)
}

/**
 * What settles a stop: the move to `stopped`, or a sub-step's failure — the
 * workspace then stays `running` (design §6) and the button is Stop again. A
 * delete overtaking it ends it too.
 */
export function settlesStop(id: string): (ev: StreamEvent) => boolean {
  return settlesBy(id, OVER.stop)
}

/** What settles a rebuild: the workspace leaving its build (`OVER.build`). */
export function settlesRebuild(id: string): (ev: StreamEvent) => boolean {
  return settlesBy(id, OVER.build)
}

/**
 * What settles a delete: `workspace.gone`, or the annotation of a delete that
 * stuck — `deleting` to `deleting` with a detail naming the sub-step, after
 * which Delete is the button again. Not the move into `deleting`: that is
 * written before the 202, and nothing has been removed yet.
 */
export function settlesDelete(id: string): (ev: StreamEvent) => boolean {
  return settlesBy(id, OVER.delete)
}

/**
 * What settles a session server start or restart (POST …/supervisor): the
 * supervisor reporting any state but `exited` — a restart reports `exited`
 * first, as it stops the old server, and that is not the end of it — or the
 * workspace leaving `running`, which ends any session server with it.
 *
 * Only the second half has a snapshot form: the server writes nothing about
 * the supervisor before answering 202, so a body showing it `serving` cannot
 * say whether that is before the restart or after it. A session mark whose
 * settling event fell in a resync gap waits for the next supervisor.state.
 */
export function settlesSession(id: string): (ev: StreamEvent) => boolean {
  const workspaceOver = settlesBy(id, OVER.session)
  return (ev) => {
    if (workspaceOver(ev)) return true
    return ev.workspace_id === id && ev.kind === 'supervisor.state' && typeof ev.data?.state === 'string' &&
      ev.data.state !== 'exited'
  }
}

// A list load in progress is joined, and one asked for meanwhile runs once
// after it — the catalog's rule.
let listInFlight: Promise<void> | null = null
let listAgain = false

export const useWorkspacesStore = defineStore('workspaces', {
  state: () => ({
    listStatus: 'idle' as LoadStatus,
    listError: null as api.ApiError | null,
    /** Per workspace id: the detail fetch's own lifecycle. */
    detail: {} as Record<string, { status: LoadStatus; error: api.ApiError | null }>,
  }),
  actions: {
    loadList(): Promise<void> {
      if (listInFlight !== null) {
        listAgain = true
        return listInFlight
      }
      listInFlight = (async () => {
        try {
          do {
            listAgain = false
            await this.fetchList()
          } while (listAgain)
        } finally {
          listInFlight = null
        }
      })()
      return listInFlight
    },

    /** @internal */
    async fetchList(): Promise<void> {
      const stream = useStreamStore()
      const { at, tick } = stream.snapshotTag()
      if (this.listStatus !== 'ready') this.listStatus = 'loading'
      try {
        const view = await api.get<WorkspaceList>('/api/workspaces')
        stream.snapshot({ type: 'workspaces', at, view }, tick)
        this.listStatus = 'ready'
        this.listError = null
      } catch (e) {
        const err = e instanceof api.ApiError ? e : new api.ApiError(0, 'network')
        if (err.status === 401) return
        this.listError = err
        if (this.listStatus !== 'ready') this.listStatus = 'error'
      }
    },

    /**
     * GET /api/workspaces/:id, into the reducer. A 404 for a workspace the
     * entities still hold asks the list, the one snapshot that may drop a
     * workspace: the detail's own 404 is not an input to the reducer, so the
     * entity — and its buttons — would otherwise outlive the row whenever
     * its `workspace.gone` fell in a resync gap.
     */
    async loadOne(id: string): Promise<void> {
      const stream = useStreamStore()
      const { at, tick } = stream.snapshotTag()
      const prev = this.detail[id]
      if (prev?.status !== 'ready') this.detail = { ...this.detail, [id]: { status: 'loading', error: null } }
      try {
        const view = await api.get<WorkspaceDetail>(`/api/workspaces/${encodeURIComponent(id)}`)
        stream.snapshot({ type: 'workspace', at, view }, tick)
        this.detail = { ...this.detail, [id]: { status: 'ready', error: null } }
      } catch (e) {
        const err = e instanceof api.ApiError ? e : new api.ApiError(0, 'network')
        if (err.status === 401) return
        const status: LoadStatus = err.code === 'not_found' ? 'not_found'
          : this.detail[id]?.status === 'ready' ? 'ready' : 'error'
        this.detail = { ...this.detail, [id]: { status, error: err } }
        if (status === 'not_found' && stream.entities.workspaces[id] !== undefined) await this.loadList()
      }
    },

    /**
     * POST /api/workspaces. A second tap while one is in flight sends
     * nothing; the server's `409 in_progress` is the guard that also covers
     * a second device.
     */
    async create(repoId: number, branch?: string): Promise<void> {
      const stream = useStreamStore()
      const key = cloneKey(repoId)
      if (key in stream.inFlight) return
      stream.begin(key, settlesClone(repoId), cloneOverIn(repoId))
      try {
        await api.send('POST', '/api/workspaces', branch === undefined
          ? { repository_id: repoId }
          : { repository_id: repoId, branch })
        stream.accepted(key)
      } catch (e) {
        stream.end(key)
        throw e
      }
    },

    /** POST /api/workspaces/:id/start, for a stopped or failed workspace. */
    start(id: string): Promise<void> {
      return this.mutate(startKey(id), settlesStart(id), overIn(id, OVER.build), 'POST', `/api/workspaces/${encodeURIComponent(id)}/start`)
    },

    /** POST /api/workspaces/:id/stop, for a running workspace. */
    stop(id: string): Promise<void> {
      return this.mutate(stopKey(id), settlesStop(id), overIn(id, OVER.stop), 'POST', `/api/workspaces/${encodeURIComponent(id)}/stop`)
    },

    /** POST /api/workspaces/:id/rebuild: a new container, the clone kept. */
    rebuild(id: string): Promise<void> {
      return this.mutate(rebuildKey(id), settlesRebuild(id), overIn(id, OVER.build), 'POST', `/api/workspaces/${encodeURIComponent(id)}/rebuild`)
    },

    /** POST /api/workspaces/:id/supervisor: start, or restart, the session server. */
    restartSession(id: string): Promise<void> {
      return this.mutate(sessionKey(id), settlesSession(id), overIn(id, OVER.session), 'POST',
        `/api/workspaces/${encodeURIComponent(id)}/supervisor`)
    },

    /**
     * DELETE /api/workspaces/:id?confirm=<what was typed>. The confirm is
     * sent exactly as given — not trimmed, not case-folded — because the
     * server compares it exactly (`400 confirm_mismatch`) and is the
     * authority; the sheet's own check only keeps the button from offering a
     * request that would be refused.
     */
    remove(id: string, confirm: string): Promise<void> {
      const path = `/api/workspaces/${encodeURIComponent(id)}?confirm=${encodeURIComponent(confirm)}`
      return this.mutate(deleteKey(id), settlesDelete(id), overIn(id, OVER.delete), 'DELETE', path)
    },

    /**
     * @internal §4.2 for one request: a second tap while in flight sends
     * nothing. The mark ends on the event `settles` accepts or, once the 202
     * has landed, on a snapshot whose entities `resolved` finds the action
     * over in — each built from the action's one OVER predicate.
     */
    async mutate(
      key: string, settles: (ev: StreamEvent) => boolean, resolved: (e: Entities) => boolean,
      method: 'POST' | 'DELETE', path: string,
    ): Promise<void> {
      const stream = useStreamStore()
      if (key in stream.inFlight) return
      stream.begin(key, settles, resolved)
      try {
        await api.send(method, path)
        stream.accepted(key)
      } catch (e) {
        stream.end(key)
        throw e
      }
    },
  },
})
