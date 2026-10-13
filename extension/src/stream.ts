// The event stream in the extension host (vscode-extension §5.3): what
// EventSource does for the web UI, over the client's `stream`, with the
// failures EventSource could not tell apart classified:
//
//   - a 401 is the one 401 path (the client has already cleared the
//     credential): the stream stops and says so;
//   - no credential, or no `drydock.url`: it stops, and the caller asks;
//   - anything else — a refused connection, a 5xx, a stream that ends, or one
//     quiet for QUIET_MS (no event and no `: ping`, which the server sends
//     every 20 s) — is retried after the web UI's backoff (BACKOFF_MS).
//
// It resumes with `Last-Event-ID`: the highest of the reducer's last applied
// id and the last id the wire set, which an id-only frame (an event this
// credential may not see, §4.1) moves without an event to apply. Every open
// after the first is reported, so the caller runs its refetchers, as the web
// UI's stream store does; a `resync` frame is the caller's too.

import { ApiError } from '../../web/src/core/errors'
import type { StreamEvent } from '../../web/src/core/types'
import type { Clock, StreamResponse } from './ports'
import { SseParser } from './sse'

/** web/src/stores/stream.ts's hard-retry schedule, capped; stream.spec.ts reads it from there. */
export const BACKOFF_MS = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000] as const
/** Two missed heartbeats and change: the connection is dead even if the socket is not. */
export const QUIET_MS = 45_000

export type Phase = 'idle' | 'connecting' | 'live' | 'backoff'

export type StopReason = 'stopped' | 'unauthorized' | 'sign_in' | 'not_configured'

export interface StreamHandlers {
  /** A domain event, parsed. */
  event(ev: StreamEvent): void
  /** A named frame (`resync`, `resources`), with the last event id at it. */
  named(type: string, data: string, lastEventId: number): void
  /** A connection opened; `reopen` for every open but the first. */
  opened(reopen: boolean): void
  phase(p: Phase): void
  /** The stream stopped of its own accord, and why; never called for `stop()`. */
  ended(reason: Exclude<StopReason, 'stopped'>): void
}

export interface StreamSource {
  stream(lastEventId: number): Promise<StreamResponse>
}

export class EventStream {
  #gen = 0
  #running = false
  #opens = 0
  #backoff = 0
  #position = 0
  #current: StreamResponse | null = null
  #cancelRetry: (() => void) | null = null

  constructor(
    private readonly source: StreamSource,
    private readonly clock: Clock,
    private readonly h: StreamHandlers,
    /** The reducer's last applied event id. */
    private readonly applied: () => number,
  ) {}

  get running(): boolean {
    return this.#running
  }

  /** Opens the stream if it is not open. Idempotent. */
  start(): void {
    if (this.#running) return
    this.#running = true
    this.#opens = 0
    this.#backoff = 0
    void this.#connect(++this.#gen)
  }

  /** Closes the stream and every timer. */
  stop(): void {
    this.#halt()
    this.h.phase('idle')
  }

  #halt(): void {
    this.#gen++
    this.#running = false
    this.#current?.cancel()
    this.#current = null
    this.#cancelRetry?.()
    this.#cancelRetry = null
  }

  async #connect(gen: number): Promise<void> {
    this.h.phase('connecting')
    let resp: StreamResponse
    try {
      resp = await this.source.stream(Math.max(this.#position, this.applied()))
    } catch (e) {
      if (gen !== this.#gen) return
      if (e instanceof ApiError && e.status === 401) return this.#end('unauthorized')
      if (e instanceof Error && e.name === 'SignInNeeded') return this.#end('sign_in')
      if (e instanceof Error && e.name === 'NotConfigured') return this.#end('not_configured')
      return this.#retry(gen)
    }
    if (gen !== this.#gen) {
      resp.cancel()
      return
    }
    this.#current = resp
    this.#opens++
    this.#backoff = 0
    this.h.phase('live')
    this.h.opened(this.#opens > 1)
    await this.#read(gen, resp)
    if (gen === this.#gen) this.#retry(gen)
  }

  async #read(gen: number, resp: StreamResponse): Promise<void> {
    const parser = new SseParser()
    let quiet = this.clock.after(QUIET_MS, () => resp.cancel())
    try {
      for await (const chunk of resp.chunks) {
        if (gen !== this.#gen) return
        quiet()
        quiet = this.clock.after(QUIET_MS, () => resp.cancel())
        for (const f of parser.feed(chunk)) {
          if (gen !== this.#gen) return
          if (f.kind === 'comment') continue
          this.#move(f.lastEventId)
          if (f.kind === 'id') continue
          if (f.type === 'message') {
            const ev = parseEvent(f.data)
            if (ev !== null) this.h.event(ev)
          } else {
            this.h.named(f.type, f.data, Math.max(this.#position, this.applied()))
          }
        }
      }
    } catch {
      // The connection failed mid-stream: retried by the caller.
    } finally {
      quiet()
      resp.cancel()
      if (this.#current === resp) this.#current = null
    }
  }

  #move(lastEventId: string): void {
    const n = Number(lastEventId)
    if (Number.isSafeInteger(n) && n > this.#position) this.#position = n
  }

  #retry(gen: number): void {
    const delay = BACKOFF_MS[Math.min(this.#backoff, BACKOFF_MS.length - 1)]!
    this.#backoff++
    this.h.phase('backoff')
    this.#cancelRetry = this.clock.after(delay, () => {
      this.#cancelRetry = null
      if (gen === this.#gen) void this.#connect(gen)
    })
  }

  #end(reason: Exclude<StopReason, 'stopped'>): void {
    this.#halt()
    this.h.phase('idle')
    this.h.ended(reason)
  }
}

/** A domain event off the wire, or null for data that is not one (the reducer's unknown kinds pass). */
export function parseEvent(raw: string): StreamEvent | null {
  try {
    const v = JSON.parse(raw) as Partial<StreamEvent> | null
    if (v === null || typeof v !== 'object' || typeof v.id !== 'number' || typeof v.kind !== 'string') return null
    return v as StreamEvent
  } catch {
    return null
  }
}
