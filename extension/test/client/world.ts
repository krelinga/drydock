// The client tier's pieces: the extension's real client over its production
// transport, bound to an origin and a stored token, and a follower that runs
// the event stream into core's reducer and marks — the extension's own state
// machinery, minus VS Code.

import { Marks, ownState } from '../../../web/src/core/marks'
import { emptyEntities, reduce, type Action, type Entities } from '../../../web/src/core/reducer'
import type { StreamEvent } from '../../../web/src/core/types'
import { DrydockClient } from '../../src/client'
import { CredentialStore } from '../../src/credential'
import { nodeTransport } from '../../src/node/transport'
import type { Clock } from '../../src/ports'
import { originOf } from '../../src/settings'
import { EventStream, type Phase, type StopReason } from '../../src/stream'
import { MemorySecrets } from '../fakes'

export const realClock: Clock = {
  now: () => Date.now(),
  after: (ms, fn) => {
    const t = setTimeout(fn, ms)
    return () => clearTimeout(t)
  },
}

export interface Bound {
  client: DrydockClient
  secrets: MemorySecrets
  credentials: CredentialStore
  /** How many times the one 401 path ran. */
  unauthorized: () => number
  setOrigin: (o: string) => void
}

/** The extension's client for `origin`, holding `token` as stored for `storedFor` (default: the same origin). */
export function bound(origin: string, token: string | null, storedFor = origin): Bound {
  const secrets = new MemorySecrets()
  if (token !== null) secrets.value = JSON.stringify({ origin: storedFor, token })
  const credentials = new CredentialStore(secrets)
  let n = 0
  let cur = origin
  const client = new DrydockClient({
    transport: nodeTransport, credentials, origin: () => originOf(cur), onUnauthorized: () => { n++ },
  })
  return { client, secrets, credentials, unauthorized: () => n, setOrigin: (o) => { cur = o } }
}

/** The stream, run into the reducer and the marks, as the extension will. */
export class Follower {
  entities: Entities = emptyEntities()
  readonly events: StreamEvent[] = []
  readonly named: Array<{ type: string; id: number }> = []
  readonly phases: Phase[] = []
  ended: StopReason | null = null
  opens = 0
  readonly marks = new Marks(realClock, ownState())
  readonly stream: EventStream

  constructor(client: DrydockClient) {
    this.stream = new EventStream(client, realClock, {
      event: (ev) => {
        this.events.push(ev)
        this.dispatch({ type: 'event', event: ev })
        this.marks.onEvent(ev)
      },
      named: (type, data, id) => {
        this.named.push({ type, id })
        if (type === 'resync') this.dispatch({ type: 'resync', id })
        else if (type === 'resources') {
          try {
            this.dispatch({ type: 'resources', frame: JSON.parse(data) })
          } catch {
            /* not ours to judge */
          }
        }
      },
      opened: () => { this.opens++ },
      phase: (p) => this.phases.push(p),
      ended: (r) => { this.ended = r },
    }, () => this.entities.lastEventId)
  }

  dispatch(a: Action): void {
    this.entities = reduce(this.entities, a)
  }

  start(): this {
    this.stream.start()
    return this
  }

  stop(): void {
    this.stream.stop()
  }

  kinds(): string[] {
    return this.events.map((e) => e.kind)
  }
}

/** Polls `ok` until true; throws naming `what`. */
export async function until(what: string, ok: () => boolean, ms = 30_000): Promise<void> {
  const deadline = Date.now() + ms
  while (!ok()) {
    if (Date.now() > deadline) throw new Error(`${what}: never`)
    await new Promise((r) => setTimeout(r, 25))
  }
}
