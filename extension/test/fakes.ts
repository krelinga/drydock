// Fakes for the ports (src/ports.ts), shared by the unit specs. Each records
// what the code under test did, so a negative assertion ("sent nothing") has
// a record to be read from.

import type { Clock } from '../../web/src/core/marks'
import type {
  HttpRequest, HttpResponse, Level, Log, Opener, Prompter, Secrets, Settings, StreamResponse, Transport, Window,
} from '../src/ports'

export const TOKEN = 'drydock_' + 'A'.repeat(20) + '_-' + 'b'.repeat(21)
export const TOKEN2 = 'drydock_' + 'Z'.repeat(43)
export const ORIGIN = 'https://drydock.test'

export function headers(h: Record<string, string> = {}) {
  const lower = Object.fromEntries(Object.entries(h).map(([k, v]) => [k.toLowerCase(), v]))
  return { get: (n: string) => lower[n.toLowerCase()] ?? null }
}

export function jsonResponse(status: number, body: unknown, extra: Record<string, string> = {}): HttpResponse {
  return { status, headers: headers({ 'Content-Type': 'application/json', ...extra }), text: async () => JSON.stringify(body) }
}

export function errorResponse(status: number, code: string, extra: Record<string, string> = {}): HttpResponse {
  return jsonResponse(status, { error: { code, message: 'prose' } }, extra)
}

export type Route = (req: HttpRequest) => HttpResponse | Promise<HttpResponse>

/** A Transport answering by `route`, recording every request. */
export class FakeTransport implements Transport {
  requests: HttpRequest[] = []
  streams: HttpRequest[] = []
  route: Route = () => jsonResponse(200, {})
  streamRoute: (req: HttpRequest) => StreamResponse | Promise<StreamResponse> = () => new FakeStream()

  async request(req: HttpRequest): Promise<HttpResponse> {
    this.requests.push(req)
    return this.route(req)
  }

  async stream(req: HttpRequest): Promise<StreamResponse> {
    this.streams.push(req)
    return this.streamRoute(req)
  }
}

/** A stream body the spec writes to chunk by chunk. */
export class FakeStream implements StreamResponse {
  cancelled = false
  #queue: string[] = []
  #wake: (() => void) | null = null
  #ended = false
  #fail = false

  constructor(readonly status = 200, readonly headers = headersOf({ 'Content-Type': 'text/event-stream' })) {}

  push(chunk: string): void {
    this.#queue.push(chunk)
    this.#wake?.()
  }

  end(): void {
    this.#ended = true
    this.#wake?.()
  }

  fail(): void {
    this.#fail = true
    this.#wake?.()
  }

  cancel(): void {
    this.cancelled = true
    this.end()
  }

  get chunks(): AsyncIterable<string> {
    const self = this
    return {
      async *[Symbol.asyncIterator]() {
        for (;;) {
          while (self.#queue.length > 0) yield self.#queue.shift()!
          if (self.#fail) throw new Error('connection reset')
          if (self.#ended) return
          await new Promise<void>((r) => { self.#wake = r })
          self.#wake = null
        }
      },
    }
  }
}

function headersOf(h: Record<string, string>) {
  return headers(h)
}

export class MemorySecrets implements Secrets {
  value: string | undefined
  writes = 0
  async get() {
    return this.value
  }
  async store(v: string) {
    this.value = v
    this.writes++
  }
  async delete() {
    this.value = undefined
  }
}

/** A Prompter that answers from a script and records what it was shown. */
export class ScriptedPrompter implements Prompter {
  shown: Array<{ level: Level; text: string; actions: readonly string[] }> = []
  inputs: Array<{ prompt: string; password?: boolean }> = []
  /** Answers to message() calls, in order; undefined dismisses. */
  answers: Array<string | undefined> = []
  /** Answers to input() calls, in order. */
  typed: Array<string | undefined> = []

  async message(level: Level, text: string, actions: readonly string[] = []) {
    this.shown.push({ level, text, actions })
    return this.answers.shift()
  }

  async input(opts: { prompt: string; password?: boolean }) {
    this.inputs.push(opts)
    return this.typed.shift()
  }

  texts(): string[] {
    return this.shown.map((s) => s.text)
  }
}

export class FakeWindow implements Window {
  constructor(public name: string | undefined = undefined, public hasFocus = true) {}
  remoteName() {
    return this.name
  }
  focused() {
    return this.hasFocus
  }
}

export class RecordingOpener implements Opener {
  opened: string[] = []
  async openExternal(url: string) {
    this.opened.push(url)
  }
}

export class MemoryLog implements Log {
  lines: string[] = []
  info(l: string) {
    this.lines.push('info ' + l)
  }
  warn(l: string) {
    this.lines.push('warn ' + l)
  }
}

export function settingsOf(url: string): Settings & { set(u: string): void } {
  let cur = url
  return { url: () => cur, set: (u: string) => { cur = u } }
}

/** A clock the spec advances by hand. */
export class FakeClock implements Clock {
  t = 0
  #timers: Array<{ at: number; fn: () => void; live: boolean }> = []
  now() {
    return this.t
  }
  after(ms: number, fn: () => void) {
    const timer = { at: this.t + ms, fn, live: true }
    this.#timers.push(timer)
    return () => { timer.live = false }
  }
  /** Moves time on, running each timer due, in order. */
  advance(ms: number) {
    const until = this.t + ms
    for (;;) {
      const due = this.#timers.filter((x) => x.live && x.at <= until).sort((a, b) => a.at - b.at)[0]
      if (due === undefined) break
      this.t = due.at
      due.live = false
      due.fn()
    }
    this.t = until
  }
  pending(): number {
    return this.#timers.filter((x) => x.live).length
  }
}

/** Lets every pending promise callback run. */
export async function flush(): Promise<void> {
  for (let i = 0; i < 20; i++) await new Promise<void>((r) => setImmediate(r))
}
