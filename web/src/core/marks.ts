// The in-flight marks (frontend §4.2), shared by every client
// (vscode-extension §5.2): the web UI's stream store and the extension's
// stream each hold one `Marks` and delegate to it, so the rule for when a
// press is over is written once.
//
// A mark ends on its settling event (`onEvent`), and also on a snapshot that
// shows the action over (`onSnapshot`). A `resync` means the settling event
// may be in a gap no replay can reach; without the second path the mark — and
// its button — would spin until a reload. A snapshot resolves a mark only if
// it was requested after the mark's request was accepted (its 2xx arrived):
// the server commits an action's receipt before answering, so only such a
// body can tell "not begun" from "over". `snapshotTag` is what orders the
// two; call it immediately before the GET whose body `onSnapshot` applies.
//
// Never on the 202, which means only that the server accepted the request.
//
// Time is injected (`Clock`): core is compiled with no DOM and no Node types
// (core/tsconfig.json), so there is no global `setTimeout` to reach for, and a
// spec drives the "no response yet" timer on its own clock.

import type { Entities } from './reducer'
import type { StreamEvent } from './types'

/** After this long with no settling event, an in-flight action says so (§4.2). */
export const SLOW_AFTER_MS = 10_000

export interface InFlight {
  since: number
  /** No settling event for SLOW_AFTER_MS: say "no response yet", never "failed". */
  slow: boolean
}

/** The time source a `Marks` runs on. */
export interface Clock {
  now(): number
  /** Calls `fn` after `ms`; the returned function cancels it. */
  after(ms: number, fn: () => void): () => void
}

/**
 * Where the in-flight record lives. The owner holds it — the web UI as Pinia
 * state, which `$reset` can clear — and `Marks` replaces it, never mutates
 * it, on every change, so a reactive owner sees each one.
 */
export interface MarkState {
  get(): Readonly<Record<string, InFlight>>
  set(next: Readonly<Record<string, InFlight>>): void
}

/** A `MarkState` held by the `Marks` itself, for an owner with no reactive state. */
export function ownState(onChange: (next: Readonly<Record<string, InFlight>>) => void = () => {}): MarkState {
  let cur: Readonly<Record<string, InFlight>> = {}
  return { get: () => cur, set: (next) => { cur = next; onChange(next) } }
}

/** The in-flight set's rules; its record is the owner's (`MarkState`). */
export class Marks {
  readonly #settlers = new Map<string, (ev: StreamEvent) => boolean>()
  /** Per key: whether the entities show the action over. Asked after a snapshot only. */
  readonly #resolvers = new Map<string, (e: Entities) => boolean>()
  /** Per key: the tick at which its request was accepted. Absent until it is. */
  readonly #accepted = new Map<string, number>()
  readonly #slow = new Map<string, () => void>()
  /** Orders acceptances against snapshot requests; never reset, only compared. */
  #tick = 0

  readonly #clock: Clock
  readonly #state: MarkState

  constructor(clock: Clock, state: MarkState = ownState()) {
    this.#clock = clock
    this.#state = state
  }

  get inFlight(): Readonly<Record<string, InFlight>> {
    return this.#state.get()
  }

  has(key: string): boolean {
    return key in this.inFlight
  }

  /**
   * §4.2 step 1: mark `key` in flight until an event that `settledBy` accepts
   * arrives. `overIn`, when given, is the same "is it over?" asked of the
   * entities after a snapshot (`onSnapshot`): the path that ends a mark whose
   * settling event fell in a resync's gap.
   */
  begin(key: string, settledBy: (ev: StreamEvent) => boolean, overIn?: (e: Entities) => boolean): void {
    this.#state.set({ ...this.inFlight, [key]: { since: this.#clock.now(), slow: false } })
    this.#settlers.set(key, settledBy)
    this.#accepted.delete(key)
    if (overIn !== undefined) this.#resolvers.set(key, overIn)
    else this.#resolvers.delete(key)
    this.#slow.get(key)?.()
    this.#slow.set(key, this.#clock.after(SLOW_AFTER_MS, () => {
      this.#slow.delete(key)
      const cur = this.inFlight[key]
      if (cur !== undefined) this.#state.set({ ...this.inFlight, [key]: { ...cur, slow: true } })
    }))
  }

  /**
   * The mark's request was accepted (a 2xx). From now on a snapshot requested
   * later reflects at least the server's receipt of it, so it may resolve the
   * mark. The response body is not read.
   */
  accepted(key: string): void {
    if (!this.has(key)) return
    this.#accepted.set(key, ++this.#tick)
  }

  /** Clears `key`: its settling event arrived, a snapshot showed it over, or the request itself was refused. */
  end(key: string): void {
    this.#settlers.delete(key)
    this.#resolvers.delete(key)
    this.#accepted.delete(key)
    this.#slow.get(key)?.()
    this.#slow.delete(key)
    if (!this.has(key)) return
    const next = { ...this.inFlight }
    delete next[key]
    this.#state.set(next)
  }

  /** Ends every mark whose settler accepts `ev`. Call it after the reducer has applied `ev`. */
  onEvent(ev: StreamEvent): void {
    for (const [key, settles] of this.#settlers) {
      if (settles(ev)) this.end(key)
    }
  }

  /** The tick a snapshot requested now is tagged with: it postdates every acceptance so far. */
  snapshotTag(): number {
    return this.#tick
  }

  /**
   * After a snapshot tagged `tick` was applied: ends each mark whose request
   * was accepted before the snapshot was requested and whose resolver finds
   * the action over in `entities`. A mark it cannot speak for, or whose action
   * is not over, is kept — "no response yet" never turns into anything else
   * here.
   */
  onSnapshot(entities: Entities, tick: number): void {
    for (const [key, over] of this.#resolvers) {
      const at = this.#accepted.get(key)
      if (at !== undefined && at <= tick && over(entities)) this.end(key)
    }
  }

  /**
   * Forgets every settler and timer, as a closed stream must. The record
   * itself is the owner's (`MarkState`) and is left as it is: the web UI
   * keeps `inFlight` until its store is reset.
   */
  clear(): void {
    for (const cancel of this.#slow.values()) cancel()
    this.#slow.clear()
    this.#settlers.clear()
    this.#resolvers.clear()
    this.#accepted.clear()
  }
}
