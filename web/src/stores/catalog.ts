// The catalog (frontend §4.1, §5): a read model over the stream store's
// entities — repositories joined to workspace state — plus the fetch that
// feeds them.
//
// It writes no entity itself. `load()` hands the GET /api/repos body to the
// stream store as a snapshot, tagged with the stream position the request was
// made at, and the reducer merges it (reducer.ts applySnapshot). What this
// store owns is the request's own lifecycle — loading, not configured, failed
// — and the search text, which is a form field the operator is typing into
// (§4.2's third kind of legitimate local state).
//
// `refresh()` is §4.2's action lifecycle for POST /api/repos/refresh: mark
// in flight, send, discard the 202, and let `repo.refreshed` or
// `repo.refresh_failed` clear it.

import { defineStore } from 'pinia'
import * as api from '../api/client'
import type { CatalogView, InstallationView, StreamEvent } from '../api/types'
import { repoHeld, workspaceForRepo, type Repo, type Workspace } from './reducer'
import { useStreamStore } from './stream'

export type CatalogStatus = 'idle' | 'loading' | 'ready' | 'not_configured' | 'error'

export const REFRESH_KEY = 'catalog:refresh'

/**
 * Workspace states the home screen's `Running` section shows: occupying,
 * failed, or deleting — a delete may still hold a container, and one that
 * stuck is waiting on the operator, which is what the section is for.
 */
const RUNNING_SECTION = new Set(['pending', 'cloning', 'building', 'running', 'failed', 'deleting'])

export interface CatalogRow {
  repo: Repo
  workspace: Workspace | null
  /** Whether any workspace holds the repository, whatever its state: see `repoHeld`. */
  held: boolean
  installation: InstallationView | null
}

export interface RunningRow {
  workspace: Workspace
  repo: Repo | null
}

/** Whether an event makes the repository list stale. */
export function catalogEvent(ev: StreamEvent): boolean {
  return ev.kind.startsWith('repo.')
}

function settlesRefresh(ev: StreamEvent): boolean {
  return ev.kind === 'repo.refreshed' || ev.kind === 'repo.refresh_failed'
}

// A load in progress is joined, and a load asked for meanwhile runs once
// after it, so a burst of repo.* events costs at most two requests.
let inFlight: Promise<void> | null = null
let again = false

export const useCatalogStore = defineStore('catalog', {
  state: () => ({
    status: 'idle' as CatalogStatus,
    error: null as api.ApiError | null,
    query: '',
  }),
  getters: {
    rows(): CatalogRow[] {
      const e = useStreamStore().entities
      return e.repoOrder.flatMap((id) => {
        const repo = e.repos[id]
        if (repo === undefined) return []
        return [{
          repo, workspace: workspaceForRepo(e, id), held: repoHeld(e, id),
          installation: e.installations[repo.installationId] ?? null,
        }]
      })
    },
    filtered(): CatalogRow[] {
      const q = this.query.trim().toLowerCase()
      if (q === '') return this.rows
      return this.rows.filter((r) => r.repo.fullName.toLowerCase().includes(q))
    },
    running(): RunningRow[] {
      const e = useStreamStore().entities
      return Object.values(e.workspaces)
        .filter((w) => w.state !== null && RUNNING_SECTION.has(w.state))
        .sort((a, b) => b.id.localeCompare(a.id))
        .map((w) => ({ workspace: w, repo: w.repositoryId === null ? null : e.repos[w.repositoryId] ?? null }))
    },
    installations(): InstallationView[] {
      return Object.values(useStreamStore().entities.installations)
    },
    /** The last refresh's failure, or null: the list is then the last good one. */
    refreshError(): { at: string; message: string } | null {
      return useStreamStore().entities.catalogRefreshError
    },
    refreshedAt(): string | null {
      return useStreamStore().entities.catalogRefreshedAt
    },
    refreshing(): boolean {
      return REFRESH_KEY in useStreamStore().inFlight
    },
  },
  actions: {
    load(): Promise<void> {
      if (inFlight !== null) {
        again = true
        return inFlight
      }
      inFlight = (async () => {
        try {
          do {
            again = false
            await this.fetchOnce()
          } while (again)
        } finally {
          inFlight = null
        }
      })()
      return inFlight
    },

    /** @internal */
    async fetchOnce(): Promise<void> {
      const stream = useStreamStore()
      // Everything at or below this id is committed, so the body reflects it.
      const at = stream.lastEventId
      if (this.status !== 'ready') this.status = 'loading'
      try {
        const view = await api.get<CatalogView>('/api/repos')
        stream.dispatch({ type: 'snapshot', at, view })
        this.status = 'ready'
        this.error = null
      } catch (e) {
        const err = e instanceof api.ApiError ? e : new api.ApiError(0, 'network')
        if (err.status === 401) return // the 401 path has it
        if (err.code === 'app_not_configured') {
          this.status = 'not_configured'
          this.error = null
          return
        }
        // Keep what is on screen if there is anything: stale-but-labelled
        // beats blank (§4.3).
        this.error = err
        if (this.status !== 'ready') this.status = 'error'
      }
    },

    /**
     * Asks the server to re-read GitHub. Resolves when the request is
     * accepted; the in-flight mark stays until the outcome event arrives.
     * Throws (and clears the mark) only when the request itself is refused.
     */
    async refresh(): Promise<void> {
      const stream = useStreamStore()
      if (REFRESH_KEY in stream.inFlight) return
      stream.begin(REFRESH_KEY, settlesRefresh)
      try {
        await api.send('POST', '/api/repos/refresh')
      } catch (e) {
        stream.end(REFRESH_KEY)
        throw e
      }
    },
  },
})
