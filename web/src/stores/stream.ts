// The stream store (frontend §4.1, §4.3): the only writer of entity state.
//
// It owns the EventSource, the connection phase, the last event id, the
// in-flight request set, and the normalized entities. Entities change in
// exactly one place — `dispatch`, which runs the pure reducer — and the only
// inputs to it are stream events and GET snapshots. A mutation's response is
// never one of them (client.ts `send` returns nothing to apply).
//
// Reconnect handling is §2.3's table. EventSource reports every failure to
// the same `error` callback with no status, so `readyState` is what tells
// them apart:
//
//   CONNECTING  the browser is already retrying (wifi, sleep, a restart).
//               Show nothing for five seconds, then a quiet header marker.
//   CLOSED      a non-200 — most likely a 401. The browser will not retry.
//               Probe GET /api/auth/session: a 401 goes down the client's
//               one 401 path (auth.ts) and signs out; anything else is a
//               hard retry with backoff.
//
// Every `open` after the first runs the registered refetchers (the current
// route's backstop), and so does a `resync` frame and an event naming a
// workspace the entities cannot place.
//
// The EventSource, the timers and the refetchers are not state — they are not
// rendered and must not be reset by `$reset` (which would orphan an open
// connection). They live in a per-store runtime beside the store, and
// `disconnect()` is what tears them down; registry.ts calls it before reset.

import { defineStore } from 'pinia'
import * as api from '../api/client'
import type { StreamEvent } from '../api/types'
import { emptyEntities, hasStubs, reduce, type Action } from './reducer'

export type Phase =
  /** No stream: signed out, or not started. */
  | 'idle'
  /** The first connection is being made. */
  | 'connecting'
  | 'live'
  /** readyState CONNECTING after an error: the browser is retrying. */
  | 'retrying'
  /** readyState CLOSED: asking GET /api/auth/session why. */
  | 'probing'
  /** Waiting out the backoff before a hard retry. */
  | 'backoff'

/** Nothing visible for this long after a drop (§4.3). */
export const MARKER_AFTER_MS = 5_000
/** After this long with no settling event, an in-flight action says so (§4.2). */
export const SLOW_AFTER_MS = 10_000
/** Hard-retry backoff after a CLOSED stream, capped. */
export const BACKOFF_MS = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000] as const

const EVENTS_URL = '/api/events'
/** EventSource.CLOSED, spelled out so a test double need not carry the statics. */
const CLOSED = 2

export interface InFlight {
  since: number
  /** No settling event for SLOW_AFTER_MS: say "no response yet", never "failed". */
  slow: boolean
}

/** A refetch the current view needs, and which events make it owed. */
export interface Refetcher {
  refetch: () => void | Promise<void>
  /** Events that make this view's read model stale; resync and reopen always do. */
  when?: (ev: StreamEvent) => boolean
}

interface Runtime {
  es: EventSource | null
  opens: number
  backoff: number
  markerTimer: ReturnType<typeof setTimeout> | null
  retryTimer: ReturnType<typeof setTimeout> | null
  slowTimers: Map<string, ReturnType<typeof setTimeout>>
  settlers: Map<string, (ev: StreamEvent) => boolean>
  refetchers: Set<Refetcher>
  /** Bumped by disconnect, so a probe that answers late cannot revive a closed stream. */
  generation: number
}

const runtimes = new WeakMap<object, Runtime>()

function rt(store: object): Runtime {
  let r = runtimes.get(store)
  if (r === undefined) {
    r = {
      es: null, opens: 0, backoff: 0, markerTimer: null, retryTimer: null,
      slowTimers: new Map(), settlers: new Map(), refetchers: new Set(), generation: 0,
    }
    runtimes.set(store, r)
  }
  return r
}

function parseEvent(raw: string): StreamEvent | null {
  try {
    const v = JSON.parse(raw) as Partial<StreamEvent> | null
    if (v === null || typeof v !== 'object' || typeof v.id !== 'number' || typeof v.kind !== 'string') return null
    return v as StreamEvent
  } catch {
    return null
  }
}

export const useStreamStore = defineStore('stream', {
  state: () => ({
    phase: 'idle' as Phase,
    /** The header marker (§4.3): set only after MARKER_AFTER_MS of not being live. */
    reconnecting: false,
    /** Keyed like `workspace:01J…:stop` or `catalog:refresh` (§4.2 step 1). */
    inFlight: {} as Record<string, InFlight>,
    entities: emptyEntities(),
  }),
  getters: {
    lastEventId: (s) => s.entities.lastEventId,
    stale: (s) => s.entities.staleSince !== null,
  },
  actions: {
    /** Opens the stream if it is not open. Idempotent. */
    connect(): void {
      const r = rt(this)
      if (r.es !== null || r.retryTimer !== null) return
      this.open(r)
    },

    /** Closes the stream and stops every timer. The 401 path calls it (registry.ts). */
    disconnect(): void {
      const r = rt(this)
      r.generation++
      r.es?.close()
      r.es = null
      r.opens = 0
      r.backoff = 0
      if (r.markerTimer !== null) clearTimeout(r.markerTimer)
      if (r.retryTimer !== null) clearTimeout(r.retryTimer)
      r.markerTimer = null
      r.retryTimer = null
      for (const t of r.slowTimers.values()) clearTimeout(t)
      r.slowTimers.clear()
      r.settlers.clear()
      this.phase = 'idle'
      this.reconnecting = false
    },

    /** @internal */
    open(r: Runtime): void {
      // A new EventSource cannot set Last-Event-ID; the query says where this
      // client got to, so a hard retry is replayed like a blip is. The
      // browser's own reconnects send the header, which the server prefers.
      const last = this.entities.lastEventId
      const es = new EventSource(last > 0 ? `${EVENTS_URL}?last_event_id=${last}` : EVENTS_URL)
      r.es = es
      if (this.phase === 'idle') this.phase = 'connecting'
      es.addEventListener('open', () => {
        if (r.es !== es) return
        r.opens++
        r.backoff = 0
        this.phase = 'live'
        this.clearMarker(r)
        // Replay closed the gap through the reducer already — the browser's
        // Last-Event-ID, or the query on a hard retry. Refetch anyway: replay
        // is bounded, and a resync is not always noticed in time.
        if (r.opens > 1) this.runRefetchers()
      })
      es.addEventListener('message', (m) => {
        if (r.es !== es) return
        const ev = parseEvent((m as MessageEvent<string>).data)
        if (ev !== null) this.receive(ev)
      })
      es.addEventListener('resync', (m) => {
        if (r.es !== es) return
        const id = Number((m as MessageEvent<string>).lastEventId)
        this.dispatch({ type: 'resync', id: Number.isFinite(id) ? id : this.entities.lastEventId })
        this.runRefetchers()
      })
      es.addEventListener('error', () => {
        if (r.es !== es) return
        this.startMarker(r)
        if (es.readyState === CLOSED) {
          es.close()
          r.es = null
          void this.probe(r)
        } else {
          this.phase = 'retrying'
        }
      })
    },

    /** @internal CLOSED: find out whether it was a 401. */
    async probe(r: Runtime): Promise<void> {
      this.phase = 'probing'
      const gen = r.generation
      try {
        await api.get('/api/auth/session')
      } catch (e) {
        // A 401 has already gone down the client's 401 path, which
        // disconnects; there is nothing to retry into.
        if (e instanceof api.ApiError && e.status === 401) return
      }
      if (gen !== r.generation) return
      const delay = BACKOFF_MS[Math.min(r.backoff, BACKOFF_MS.length - 1)]!
      r.backoff++
      this.phase = 'backoff'
      r.retryTimer = setTimeout(() => {
        r.retryTimer = null
        if (gen === r.generation) this.open(r)
      }, delay)
    },

    /** @internal */
    startMarker(r: Runtime): void {
      if (r.markerTimer !== null || this.reconnecting) return
      r.markerTimer = setTimeout(() => {
        r.markerTimer = null
        if (this.phase !== 'live') this.reconnecting = true
      }, MARKER_AFTER_MS)
    },

    /** @internal */
    clearMarker(r: Runtime): void {
      if (r.markerTimer !== null) clearTimeout(r.markerTimer)
      r.markerTimer = null
      this.reconnecting = false
    },

    /** One event off the wire: reduce, settle in-flight actions, and refetch if owed. */
    receive(ev: StreamEvent): void {
      const before = this.entities
      this.dispatch({ type: 'event', event: ev })
      const r = rt(this)
      for (const [key, settles] of r.settlers) {
        if (settles(ev)) this.end(key)
      }
      const unplaced = this.entities !== before && hasStubs(this.entities) && !hasStubs(before)
      for (const f of r.refetchers) {
        if (unplaced || f.when?.(ev) === true) void f.refetch()
      }
    },

    /** The single write path into entities. */
    dispatch(action: Action): void {
      this.entities = reduce(this.entities, action)
    },

    /** @internal */
    runRefetchers(): void {
      for (const f of rt(this).refetchers) void f.refetch()
    },

    /** Registers the current view's backstop refetch; returns the unregister. */
    addRefetcher(f: Refetcher): () => void {
      const r = rt(this)
      r.refetchers.add(f)
      return () => r.refetchers.delete(f)
    },

    /**
     * §4.2 step 1: mark `key` in flight until an event that `settledBy`
     * accepts arrives. Not until the response lands — the 202 means only that
     * the server accepted it.
     */
    begin(key: string, settledBy: (ev: StreamEvent) => boolean): void {
      const r = rt(this)
      this.inFlight = { ...this.inFlight, [key]: { since: Date.now(), slow: false } }
      r.settlers.set(key, settledBy)
      const old = r.slowTimers.get(key)
      if (old !== undefined) clearTimeout(old)
      r.slowTimers.set(key, setTimeout(() => {
        r.slowTimers.delete(key)
        const cur = this.inFlight[key]
        if (cur !== undefined) this.inFlight = { ...this.inFlight, [key]: { ...cur, slow: true } }
      }, SLOW_AFTER_MS))
    },

    /** Clears `key`: its settling event arrived, or the request itself was refused. */
    end(key: string): void {
      const r = rt(this)
      r.settlers.delete(key)
      const t = r.slowTimers.get(key)
      if (t !== undefined) clearTimeout(t)
      r.slowTimers.delete(key)
      if (!(key in this.inFlight)) return
      const next = { ...this.inFlight }
      delete next[key]
      this.inFlight = next
    },
  },
})
