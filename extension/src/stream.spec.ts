// The stream's connection handling (stream.ts; vscode-extension §5.3) on a
// fake clock: a 401 ends it, anything else is retried on the web UI's
// schedule, a quiet stream is dropped, and it resumes from the highest id it
// has seen, an id-only frame's included.

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { ApiError } from '../../web/src/core/errors'
import type { StreamEvent } from '../../web/src/core/types'
import { SignInNeeded } from './client'
import { BACKOFF_MS, EventStream, QUIET_MS, type Phase, type StreamHandlers } from './stream'
import { FakeClock, FakeStream, flush } from '../test/fakes'

const ev = (id: number) => JSON.stringify({ id, kind: 'workspace.state', workspace_id: 'w', data: { state: 'running' } })

function setup() {
  const clock = new FakeClock()
  const opened: number[] = []
  const streams: FakeStream[] = []
  const answers: Array<() => FakeStream | Error> = []
  const seen: StreamEvent[] = []
  const named: Array<[string, number]> = []
  const reopens: boolean[] = []
  const phases: Phase[] = []
  const ended: string[] = []
  let applied = 0
  const source = {
    async stream(last: number) {
      opened.push(last)
      const next = answers.shift() ?? (() => new FakeStream())
      const r = next()
      if (r instanceof Error) throw r
      streams.push(r)
      return r
    },
  }
  const h: StreamHandlers = {
    event: (e) => {
      seen.push(e)
      applied = Math.max(applied, e.id)
    },
    named: (t, _d, id) => named.push([t, id]),
    opened: (re) => reopens.push(re),
    phase: (p) => phases.push(p),
    ended: (r) => ended.push(r),
  }
  const s = new EventStream(source, clock, h, () => applied)
  return { s, clock, opened, streams, answers, seen, named, reopens, phases, ended }
}

describe('EventStream', () => {
  it('reads events, skips heartbeats, and reports a resync with its id', async () => {
    const t = setup()
    t.s.start()
    await flush()
    t.streams[0]!.push(`: ping\n\nid: 1\ndata: ${ev(1)}\n\nid: 2\nevent: resync\ndata: {}\n\n`)
    await flush()
    expect(t.seen.map((e) => e.id)).toEqual([1])
    expect(t.named).toEqual([['resync', 2]])
    expect(t.reopens).toEqual([false])
  })

  it('a dropped stream is retried after the backoff, resuming from the highest id seen, id-only frames included', async () => {
    const t = setup()
    t.s.start()
    await flush()
    t.streams[0]!.push(`id: 5\ndata: ${ev(5)}\n\nid: 9\n\n`)
    await flush()
    t.streams[0]!.end()
    await flush()
    expect(t.phases.at(-1)).toBe('backoff')
    t.clock.advance(BACKOFF_MS[0] - 1)
    await flush()
    expect(t.opened).toEqual([0])
    t.clock.advance(1)
    await flush()
    expect(t.opened).toEqual([0, 9])
    expect(t.reopens).toEqual([false, true])
  })

  it('the backoff grows on repeated failures and resets on an open', async () => {
    const t = setup()
    const down = () => new ApiError(0, 'network')
    t.answers.push(down, down, down)
    t.s.start()
    await flush()
    for (const d of BACKOFF_MS.slice(0, 3)) {
      const before = t.opened.length
      t.clock.advance(d - 1)
      await flush()
      expect(t.opened.length).toBe(before)
      t.clock.advance(1)
      await flush()
      expect(t.opened.length).toBe(before + 1)
    }
    // The fourth attempt opened: a later drop waits the first step again.
    t.streams.at(-1)!.end()
    await flush()
    t.clock.advance(BACKOFF_MS[0])
    await flush()
    expect(t.opened).toHaveLength(5)
  })

  it('a stream quiet for QUIET_MS is dropped and reopened; one with heartbeats is kept (control)', async () => {
    const t = setup()
    t.s.start()
    await flush()
    for (let i = 0; i < 4; i++) {
      t.clock.advance(QUIET_MS - 1_000)
      t.streams[0]!.push(': ping\n\n')
      await flush()
    }
    expect(t.streams[0]!.cancelled).toBe(false)
    t.clock.advance(QUIET_MS)
    await flush()
    expect(t.streams[0]!.cancelled).toBe(true)
    t.clock.advance(BACKOFF_MS[0])
    await flush()
    expect(t.opened).toHaveLength(2)
  })

  it('a 401 ends it for good; a 503 is retried (control)', async () => {
    const t = setup()
    t.answers.push(() => new ApiError(503, 'unavailable'), () => new ApiError(401, 'unauthenticated'))
    t.s.start()
    await flush()
    expect(t.ended).toEqual([])
    t.clock.advance(BACKOFF_MS[0])
    await flush()
    expect(t.ended).toEqual(['unauthorized'])
    expect(t.s.running).toBe(false)
    t.clock.advance(60_000)
    await flush()
    expect(t.opened).toHaveLength(2)
    expect(t.clock.pending()).toBe(0)
  })

  it('no credential ends it, asking for a sign-in', async () => {
    const t = setup()
    t.answers.push(() => new SignInNeeded('none'))
    t.s.start()
    await flush()
    expect(t.ended).toEqual(['sign_in'])
  })

  it('stop closes the connection and every timer; nothing reopens', async () => {
    const t = setup()
    t.s.start()
    await flush()
    t.s.stop()
    await flush()
    expect(t.streams[0]!.cancelled).toBe(true)
    t.clock.advance(120_000)
    await flush()
    expect(t.opened).toHaveLength(1)
    expect(t.phases.at(-1)).toBe('idle')
  })

  it("the backoff is the web UI's", () => {
    const web = readFileSync(resolve(process.cwd(), '../web/src/stores/stream.ts'), 'utf8')
    const m = /export const BACKOFF_MS = \[([^\]]+)\]/.exec(web)
    expect(m).not.toBeNull()
    expect(m![1]!.split(',').map((x) => Number(x.trim().replace(/_/g, '')))).toEqual([...BACKOFF_MS])
  })
})
