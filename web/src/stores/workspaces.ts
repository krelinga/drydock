// The workspaces store (frontend §4.1): the fetches that feed workspace
// entities, and the mutations — create and start (Phase 2), stop, rebuild
// and delete (Phase 6), and the host-access approval (design §6).
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
// action is over, not merely begun — or, for an action the server runs as a
// job, that job's own end: the one `workspace.job` event of its kind every
// job ends with, whichever path it took (`settlesJob`).

import { defineStore } from 'pinia'
import * as api from '../api/client'
import type { StreamEvent, WorkspaceDetail, WorkspaceList } from '../core/types'
import type { Entities } from '../core/reducer'
import {
  OVER, cloneOverIn, overIn, settlesApprove, settlesClone, settlesDecline, settlesDelete, settlesJobOr, settlesRebuild,
  settlesSession, settlesStart, settlesStop, type JobKind,
} from '../core/settle'
import { useStreamStore } from './stream'

// The settling predicates live in core (vscode-extension §5.2), shared with
// the extension; re-exported so no call site changes.
export {
  OVER, cloneOverIn, entityOutcome, eventOutcome, settlesApprove, settlesClone, settlesDecline, settlesDelete, settlesJob,
  settlesRebuild, settlesSession, settlesStart, settlesStop, type JobKind, type Outcome,
} from '../core/settle'

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
/** The in-flight key for approving a workspace's host-access request. */
export const approveKey = (id: string) => `workspace:${id}:approve`
/** The in-flight key for declining it. */
export const declineKey = (id: string) => `workspace:${id}:decline`

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
      return this.mutate(startKey(id), [id, 'start'], settlesStart(id), overIn(id, OVER.build), 'POST', `/api/workspaces/${encodeURIComponent(id)}/start`)
    },

    /** POST /api/workspaces/:id/stop, for a running workspace. */
    stop(id: string): Promise<void> {
      return this.mutate(stopKey(id), [id, 'stop'], settlesStop(id), overIn(id, OVER.stop), 'POST', `/api/workspaces/${encodeURIComponent(id)}/stop`)
    },

    /** POST /api/workspaces/:id/rebuild: a new container, the clone kept. */
    rebuild(id: string): Promise<void> {
      return this.mutate(rebuildKey(id), [id, 'rebuild'], settlesRebuild(id), overIn(id, OVER.build), 'POST', `/api/workspaces/${encodeURIComponent(id)}/rebuild`)
    },

    /** POST /api/workspaces/:id/supervisor: start, or restart, the session server. */
    restartSession(id: string): Promise<void> {
      return this.mutate(sessionKey(id), [id, 'supervisor'], settlesSession(id), overIn(id, OVER.session), 'POST',
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
      return this.mutate(deleteKey(id), [id, 'delete'], settlesDelete(id), overIn(id, OVER.delete), 'DELETE', path)
    },

    /**
     * POST /api/workspaces/:id/config-approval with the hash the page showed:
     * the server approves only that request, so a configuration that changed
     * since is `409 approval_stale`, never approved unseen (design §6). It
     * continues the stopped start or rebuild, so it ends as one does.
     */
    approve(id: string, hash: string): Promise<void> {
      return this.mutate(approveKey(id), [id, 'approve'], settlesApprove(id), overIn(id, OVER.build), 'POST',
        `/api/workspaces/${encodeURIComponent(id)}/config-approval`, { hash })
    },

    /** DELETE /api/workspaces/:id/config-approval: the workspace stays stopped. */
    decline(id: string): Promise<void> {
      // Not a job: the decline's own state event, written before the 202, ends it.
      return this.mutate(declineKey(id), null, settlesDecline(id), overIn(id, OVER.decline), 'DELETE',
        `/api/workspaces/${encodeURIComponent(id)}/config-approval`)
    },

    /**
     * @internal §4.2 for one request: a second tap while in flight sends
     * nothing. The mark ends on the event `settles` accepts, on the end of
     * the job the request starts (`job`: its workspace and kind; null for a
     * request that runs none) or, once the 202 has landed, on a snapshot
     * whose entities `resolved` finds the action over in — each built from
     * the action's one OVER predicate.
     */
    async mutate(
      key: string, job: [string, JobKind] | null, settles: (ev: StreamEvent) => boolean, resolved: (e: Entities) => boolean,
      method: 'POST' | 'DELETE', path: string, body?: unknown,
    ): Promise<void> {
      const stream = useStreamStore()
      if (key in stream.inFlight) return
      if (job !== null) settles = settlesJobOr(job[0], job[1], stream.lastEventId, settles)
      stream.begin(key, settles, resolved)
      try {
        await api.send(method, path, body)
        stream.accepted(key)
      } catch (e) {
        stream.end(key)
        throw e
      }
    },
  },
})
