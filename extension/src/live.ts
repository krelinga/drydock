// One window's view of Drydock (vscode-extension §5): core's reducer and
// marks, fed by the event stream and by GET snapshots and by nothing else, so
// the extension follows "the frontend invents no state" with the web UI's own
// code. A mutation's response is never an input (client.ts `send` returns
// nothing).
//
// The stream is held while something needs it (§5.5) — a command running, a
// view shown, a mark in flight, a follow under way — and closed IDLE_MS after
// nothing does, so ten idle windows hold no streams. Every open refetches the
// snapshots (the server subscribes before it answers, so nothing is missed
// from there), and GET /api/meta again, since the server may have been
// upgraded underneath; a `resync` refetches too; a move to `running`
// refetches that workspace, since its VS Code link is in views, never in
// events (§2.6).

import { ApiError } from '../../web/src/core/errors'
import { Marks, ownState } from '../../web/src/core/marks'
import { becameRunning, emptyEntities, reduce, type Action, type Entities } from '../../web/src/core/reducer'
import type { CatalogView, StreamEvent, WorkspaceDetail, WorkspaceList } from '../../web/src/core/types'
import { NotConfigured, SignInNeeded, type DrydockClient } from './client'
import { parseMeta, skew, type Meta, type Skew } from './handshake'
import { errorLine } from './log'
import type { Clock, Log } from './ports'
import { pathFor } from './routes'
import { EventStream, type Phase, type StopReason } from './stream'

/** How long the stream stays open after nothing holds it (§5.5). */
export const IDLE_MS = 5 * 60_000

export interface LiveDeps {
  client: DrydockClient
  clock: Clock
  log: Log
  /** The stream stopped of its own accord (a 401, no credential, no drydock.url). */
  ended?: (reason: Exclude<StopReason, 'stopped'>) => void
}

export class Live {
  entities: Entities = emptyEntities()
  meta: Meta | null = null
  phase: Phase = 'idle'
  /** When `phase` last changed, on the injected clock: what the status bar's quiet marker reads. */
  phaseSince = 0
  readonly marks: Marks
  readonly #stream: EventStream
  readonly #listeners = new Set<() => void>()
  #holds = 0
  #idle: (() => void) | null = null
  #loading: Promise<void> | null = null

  constructor(private readonly d: LiveDeps) {
    this.marks = new Marks(d.clock, ownState(() => this.#changed()))
    this.#stream = new EventStream(d.client, d.clock, {
      event: (ev) => this.#event(ev),
      named: (type, data, id) => {
        if (type === 'resync') {
          this.dispatch({ type: 'resync', id })
          void this.refetch()
        } else if (type === 'resources') {
          try {
            this.dispatch({ type: 'resources', frame: JSON.parse(data) })
          } catch {
            /* a frame the reducer would not read either */
          }
        }
      },
      opened: () => {
        void this.refetch().then(() => this.#settle(null), (e: unknown) => this.#settle(e))
      },
      phase: (p) => {
        if (p !== this.phase) this.phaseSince = d.clock.now()
        this.phase = p
        // The first connection failed: say so to whoever waits, rather than
        // hold a command until the backoff succeeds.
        if (p === 'backoff') this.#settle(new ApiError(0, 'network', null, 'the event stream could not connect'))
        this.#changed()
      },
      ended: (reason) => {
        this.#cancelIdle()
        this.#settle(reason === 'unauthorized' ? new ApiError(401, 'unauthenticated')
          : reason === 'sign_in' ? new SignInNeeded('none') : new NotConfigured())
        d.ended?.(reason)
      },
    }, () => this.entities.lastEventId)
  }

  /** Called on every change of entities, marks or phase. Returns the unsubscribe. */
  onChange(fn: () => void): () => void {
    this.#listeners.add(fn)
    return () => this.#listeners.delete(fn)
  }

  /** The skew between this extension and the server, or null before a handshake. */
  skew(): Skew | null {
    return this.meta === null ? null : skew(this.meta.api)
  }

  /**
   * Holds the stream open until the returned release is called (§5.5), and
   * opens it if it is not open. Resolves once the first snapshots are in.
   */
  async hold(): Promise<() => void> {
    this.#holds++
    this.#cancelIdle()
    let released = false
    const release = () => {
      if (released) return
      released = true
      this.#holds--
      this.#maybeIdle()
    }
    try {
      await this.#ready()
    } catch (e) {
      release()
      throw e
    }
    return release
  }

  /** Whether the stream is held: what the status bar's idle marker reads. */
  get held(): boolean {
    return this.#holds > 0 || Object.keys(this.marks.inFlight).length > 0
  }

  get streaming(): boolean {
    return this.#stream.running
  }

  /** Every snapshot this window reads, and the handshake, again. */
  refetch(): Promise<void> {
    if (this.#loading !== null) return this.#loading
    this.#loading = (async () => {
      try {
        await Promise.all([this.#meta(), this.#snapshot('snapshot'), this.#snapshot('workspaces')])
      } finally {
        this.#loading = null
      }
    })()
    return this.#loading
  }

  /** GET /api/workspaces/:id into the reducer: the view carries the VS Code link (§2.6). */
  async loadWorkspace(id: string): Promise<void> {
    const at = this.entities.lastEventId
    const tick = this.marks.snapshotTag()
    const view = await this.d.client.get<WorkspaceDetail>(pathFor('workspaces.read', { id }))
    this.dispatch({ type: 'workspace', at, view })
    this.marks.onSnapshot(this.entities, tick)
  }

  /** The single write path into entities. */
  dispatch(a: Action): void {
    this.entities = reduce(this.entities, a)
    this.#changed()
  }

  /** Closes the stream now (sign-out, deactivate). */
  close(): void {
    this.#cancelIdle()
    this.marks.clear()
    this.#fresh = false
    this.#stream.stop()
  }

  #fresh = false
  #waiters: Array<{ resolve: () => void; reject: (e: unknown) => void }> = []

  /**
   * Resolves once the stream is open and the snapshots read after it opened
   * are applied: a body read before the stream subscribed could predate an
   * event it will never carry (frontend §4.3).
   */
  #ready(): Promise<void> {
    if (this.#stream.running && this.#fresh) return Promise.resolve()
    const p = new Promise<void>((resolve, reject) => this.#waiters.push({ resolve, reject }))
    if (!this.#stream.running) {
      this.#fresh = false
      this.#stream.start()
    }
    return p
  }

  #settle(err: unknown): void {
    if (err === null) this.#fresh = true
    const ws = this.#waiters
    this.#waiters = []
    for (const w of ws) {
      if (err === null) w.resolve()
      else w.reject(err)
    }
  }

  async #meta(): Promise<void> {
    try {
      this.meta = parseMeta(await this.d.client.get(pathFor('meta.read')))
      this.#changed()
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) throw e
      this.d.log.warn(`meta: ${errorLine(e)}`)
    }
  }

  async #snapshot(kind: 'snapshot' | 'workspaces'): Promise<void> {
    const at = this.entities.lastEventId
    const tick = this.marks.snapshotTag()
    if (kind === 'snapshot') {
      this.dispatch({ type: 'snapshot', at, view: await this.d.client.get<CatalogView>(pathFor('repos.list')) })
    } else {
      this.dispatch({ type: 'workspaces', at, view: await this.d.client.get<WorkspaceList>(pathFor('workspaces.list')) })
    }
    this.marks.onSnapshot(this.entities, tick)
  }

  #event(ev: StreamEvent): void {
    this.dispatch({ type: 'event', event: ev })
    this.marks.onEvent(ev)
    if (becameRunning(ev) && ev.workspace_id !== undefined) {
      void this.loadWorkspace(ev.workspace_id).catch((e: unknown) => this.d.log.warn(`refetching a workspace: ${errorLine(e)}`))
    }
    if (ev.kind.startsWith('repo.')) void this.#snapshot('snapshot').catch(() => {})
    this.#maybeIdle()
  }

  #changed(): void {
    for (const fn of this.#listeners) fn()
  }

  #maybeIdle(): void {
    if (this.held || this.#idle !== null || !this.#stream.running) return
    this.#idle = this.d.clock.after(IDLE_MS, () => {
      this.#idle = null
      if (!this.held) {
        this.d.log.info('closing the event stream: nothing has needed it for five minutes')
        this.#fresh = false
        this.#stream.stop()
      }
    })
  }

  #cancelIdle(): void {
    this.#idle?.()
    this.#idle = null
  }
}
