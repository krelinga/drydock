// An EventSource the specs drive by hand. jsdom has none, and what the stream
// store has to get right is precisely the part a real one makes hard to
// arrange: which readyState an error leaves behind (frontend §2.3), and what
// arrives in what order.
//
// It speaks the same events a browser's does — `open`, unnamed `message`,
// named ones like `resync`, and `error` — with `lastEventId` on each, so the
// store's listeners are exercised as written.

import type { MockBackend } from '../mocks/backend'
import type { StreamEvent } from '../api/types'

export class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSED = 2
  static instances: FakeEventSource[] = []

  /** The newest instance: what a hard retry would have created. */
  static latest(): FakeEventSource {
    const es = FakeEventSource.instances[FakeEventSource.instances.length - 1]
    if (es === undefined) throw new Error('no EventSource was created')
    return es
  }

  readyState = FakeEventSource.CONNECTING
  closed = false
  private unpipe: (() => void) | null = null

  constructor(readonly url: string) {
    super()
    FakeEventSource.instances.push(this)
  }

  close(): void {
    this.readyState = FakeEventSource.CLOSED
    this.closed = true
    this.unpipe?.()
  }

  /** The connection (or the browser's own reconnection) succeeded. */
  open(): this {
    this.readyState = FakeEventSource.OPEN
    this.dispatchEvent(new Event('open'))
    return this
  }

  /** One unnamed frame, as events_routes.go writes a domain event. */
  send(ev: StreamEvent): this {
    this.dispatchEvent(new MessageEvent('message', { data: JSON.stringify(ev), lastEventId: String(ev.id) }))
    return this
  }

  /** A raw frame, for malformed data. */
  sendRaw(data: string, id = ''): this {
    this.dispatchEvent(new MessageEvent('message', { data, lastEventId: id }))
    return this
  }

  /** The named `resync` frame, carrying the latest id. */
  resync(latest: number): this {
    this.dispatchEvent(new MessageEvent('resync', { data: '{}', lastEventId: String(latest) }))
    return this
  }

  /** A network drop: the browser is already retrying (readyState CONNECTING). */
  drop(): this {
    this.readyState = FakeEventSource.CONNECTING
    this.dispatchEvent(new Event('error'))
    return this
  }

  /** A non-200 (a 401, say): the connection fails for good (readyState CLOSED). */
  refuse(): this {
    this.readyState = FakeEventSource.CLOSED
    this.dispatchEvent(new Event('error'))
    return this
  }

  /** Delivers everything the mock backend emits from now on, as the server would. */
  pipe(b: MockBackend): this {
    const sub = (ev: StreamEvent) => {
      if (!this.closed) this.send(ev)
    }
    b.subscribers.add(sub)
    this.unpipe = () => b.subscribers.delete(sub)
    return this
  }
}

export function installFakeEventSource(): void {
  ;(globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource
}
