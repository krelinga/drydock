// The workspaces store (frontend §4.1): the fetches that feed workspace
// entities, and the two Phase 2 mutations, create and start.
//
// Like the catalog it writes no entity itself. GET /api/workspaces and GET
// /api/workspaces/:id are handed to the stream store as snapshots tagged with
// the stream position they were asked at, and the reducer merges them. What
// this store owns is each request's own lifecycle — loading, not found,
// failed — which is not entity state.
//
// Both mutations are §4.2 to the letter: mark in flight, send, discard the
// 202, and let an event clear the mark. They resolve when the server accepts
// and throw (clearing the mark) only when it refuses.

import { defineStore } from 'pinia'
import * as api from '../api/client'
import type { StreamEvent, WorkspaceDetail, WorkspaceList } from '../api/types'
import { useStreamStore } from './stream'

export type LoadStatus = 'idle' | 'loading' | 'ready' | 'not_found' | 'error'

/** The in-flight key for cloning a repository (§4.2 step 1). */
export const cloneKey = (repoId: number) => `repo:${repoId}:clone`
/** The in-flight key for starting a workspace. */
export const startKey = (id: string) => `workspace:${id}:start`

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

/** What settles a start: the workspace's state or steps moving at all. */
export function settlesStart(id: string): (ev: StreamEvent) => boolean {
  return (ev) => ev.workspace_id === id && (ev.kind === 'workspace.state' || ev.kind === 'workspace.step')
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
      const at = stream.lastEventId
      if (this.listStatus !== 'ready') this.listStatus = 'loading'
      try {
        const view = await api.get<WorkspaceList>('/api/workspaces')
        stream.dispatch({ type: 'workspaces', at, view })
        this.listStatus = 'ready'
        this.listError = null
      } catch (e) {
        const err = e instanceof api.ApiError ? e : new api.ApiError(0, 'network')
        if (err.status === 401) return
        this.listError = err
        if (this.listStatus !== 'ready') this.listStatus = 'error'
      }
    },

    /** GET /api/workspaces/:id, into the reducer. */
    async loadOne(id: string): Promise<void> {
      const stream = useStreamStore()
      const at = stream.lastEventId
      const prev = this.detail[id]
      if (prev?.status !== 'ready') this.detail = { ...this.detail, [id]: { status: 'loading', error: null } }
      try {
        const view = await api.get<WorkspaceDetail>(`/api/workspaces/${encodeURIComponent(id)}`)
        stream.dispatch({ type: 'workspace', at, view })
        this.detail = { ...this.detail, [id]: { status: 'ready', error: null } }
      } catch (e) {
        const err = e instanceof api.ApiError ? e : new api.ApiError(0, 'network')
        if (err.status === 401) return
        const status: LoadStatus = err.code === 'not_found' ? 'not_found'
          : this.detail[id]?.status === 'ready' ? 'ready' : 'error'
        this.detail = { ...this.detail, [id]: { status, error: err } }
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
      stream.begin(key, settlesClone(repoId))
      try {
        await api.send('POST', '/api/workspaces', branch === undefined
          ? { repository_id: repoId }
          : { repository_id: repoId, branch })
      } catch (e) {
        stream.end(key)
        throw e
      }
    },

    /** POST /api/workspaces/:id/start, for a stopped or failed workspace. */
    async start(id: string): Promise<void> {
      const stream = useStreamStore()
      const key = startKey(id)
      if (key in stream.inFlight) return
      stream.begin(key, settlesStart(id))
      try {
        await api.send('POST', `/api/workspaces/${encodeURIComponent(id)}/start`)
      } catch (e) {
        stream.end(key)
        throw e
      }
    },
  },
})
