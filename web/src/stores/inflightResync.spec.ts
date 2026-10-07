// In-flight marks after a resync (#20, #33's review): a mark ends on its
// settling event, and — when that event fell in a gap no replay can reach —
// on the first snapshot that shows the action over. Before this, a resync
// left the mark, and its button, spinning until a reload, and a second tap
// sent nothing.
//
// The mock's manual mode holds a script's first event too, which the real
// server commits before answering 202; `receipt` plays it while the stream
// is still attached, which is the order the server's own events arrive in.

import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import * as api from '../api/client'
import type { WorkspaceList } from '../api/types'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, settle, useMockApi } from '../test/setup'
import { playScript, WS_FAILED, WS_RUNNING, type MockBackend } from '../mocks/backend'
import { useStreamStore } from './stream'
import {
  OVER, cloneKey, deleteKey, entityOutcome, rebuildKey, startKey, stopKey, useWorkspacesStore, type Outcome,
} from './workspaces'

useMockApi()

beforeEach(() => {
  setActivePinia(createPinia())
})

afterEach(() => {
  useStreamStore().disconnect()
})

const posts = (b: MockBackend, method: string, path: string) =>
  b.log.filter((r) => r.method === method && new URL(r.url).pathname === path).length

/** A live stream on the backend, and a way to cut it so events fall in a gap. */
async function live(b: MockBackend) {
  const stream = useStreamStore()
  const ws = useWorkspacesStore()
  stream.connect()
  const es = FakeEventSource.latest().open().pipe(b)
  await ws.loadList()
  return {
    stream, ws, es,
    /** What happens next is not delivered: the phone slept, and the server says resync. */
    gap(play: () => void) {
      es.unpipe?.()
      play()
      es.resync(b.events[b.events.length - 1]!.id)
      es.pipe(b)
    },
  }
}

describe('a mark whose settling event fell in a resync gap', () => {
  it('is resolved by the refetched list, and the same action is sent again (the reviewer\'s reproduction)', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { stream, ws, gap } = await live(b)
    await ws.stop(WS_RUNNING)
    playScript(b, WS_RUNNING, 1) // the receipt, seen live
    expect(stopKey(WS_RUNNING) in stream.inFlight).toBe(true)

    // The rest of the stop — stopped, its settling event — is never delivered.
    gap(() => playScript(b, WS_RUNNING))
    expect(stream.stale).toBe(true)
    expect(stopKey(WS_RUNNING) in stream.inFlight).toBe(true) // the resync alone decides nothing
    await ws.loadList()
    expect(stream.entities.workspaces[WS_RUNNING]?.state).toBe('stopped')
    expect(stopKey(WS_RUNNING) in stream.inFlight).toBe(false)

    // Started again elsewhere; a second Stop is a second request, not a dead tap.
    b.workspaces[WS_RUNNING] = { ...b.workspaces[WS_RUNNING]!, state: 'running' }
    await ws.loadList()
    await ws.stop(WS_RUNNING)
    expect(posts(b, 'POST', `/api/workspaces/${WS_RUNNING}/stop`)).toBe(2)
  })

  it('stays in flight while the snapshot shows the action still going; "no response yet" is never a verdict', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { stream, ws, gap } = await live(b)
    await ws.stop(WS_RUNNING)
    playScript(b, WS_RUNNING, 1)
    gap(() => playScript(b, WS_RUNNING, 2)) // two sub-steps; still running
    await ws.loadList()
    expect(stopKey(WS_RUNNING) in stream.inFlight).toBe(true)
    expect(stream.inFlight[stopKey(WS_RUNNING)]).toMatchObject({ slow: false })
    // Control: the stop's own settling event, live, still ends it — the event path is unchanged.
    playScript(b, WS_RUNNING)
    await settle()
    expect(stopKey(WS_RUNNING) in stream.inFlight).toBe(false)
  })

  // Each action, both ways round: after its receipt a snapshot keeps the
  // mark (the action is under way), and after the gap a snapshot that shows
  // its end clears it.
  const cases: Array<{
    name: string; key: string; setup?: (b: MockBackend) => void
    run: (ws: ReturnType<typeof useWorkspacesStore>) => Promise<void>; id: string
    /** Whether the receipt alone ends it: a clone's receipt is its settling event. */
    receiptEnds?: boolean
  }> = [
    { name: 'stop', key: stopKey(WS_RUNNING), id: WS_RUNNING, run: (ws) => ws.stop(WS_RUNNING) },
    {
      name: 'a stop that fails', key: stopKey(WS_RUNNING), id: WS_RUNNING, run: (ws) => ws.stop(WS_RUNNING),
      setup: (b) => { b.failAction = 'container' },
    },
    { name: 'rebuild', key: rebuildKey(WS_RUNNING), id: WS_RUNNING, run: (ws) => ws.rebuild(WS_RUNNING) },
    { name: 'start', key: startKey(WS_FAILED), id: WS_FAILED, run: (ws) => ws.start(WS_FAILED) },
    { name: 'delete', key: deleteKey(WS_RUNNING), id: WS_RUNNING, run: (ws) => ws.remove(WS_RUNNING, 'krelinga/drydock') },
    {
      name: 'a delete that sticks', key: deleteKey(WS_RUNNING), id: WS_RUNNING,
      run: (ws) => ws.remove(WS_RUNNING, 'krelinga/drydock'), setup: (b) => { b.failAction = 'files' },
    },
  ]
  for (const c of cases) {
    it(`${c.name}: kept while under way, resolved once the snapshot shows it over`, async () => {
      const b = freshBackend({ signedIn: true, scriptMode: 'manual', capacity: 10 })
      c.setup?.(b)
      const { stream, ws, gap } = await live(b)
      await c.run(ws)
      playScript(b, c.id, 1) // the receipt the server commits before its 202
      await settle()
      expect(c.key in stream.inFlight).toBe(true)
      await ws.loadList()
      expect(c.key in stream.inFlight).toBe(true)
      gap(() => playScript(b, c.id))
      await ws.loadList()
      expect(c.key in stream.inFlight).toBe(false)
    })
  }

  it('clone: resolved by the list once the repository is held', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { stream, ws, gap } = await live(b)
    await ws.create(3)
    await ws.loadList()
    expect(cloneKey(3) in stream.inFlight).toBe(true) // the create has not landed yet
    gap(() => playScript(b, Object.keys(b.scripts).find((id) => b.scripts[id]!.length > 0)!))
    await ws.loadList()
    expect(cloneKey(3) in stream.inFlight).toBe(false)
  })

  it('the detail view\'s snapshot resolves it too, and a 404 there asks the list, which drops the workspace', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { stream, ws, gap } = await live(b)
    await ws.remove(WS_RUNNING, 'krelinga/drydock')
    playScript(b, WS_RUNNING, 1)
    gap(() => playScript(b, WS_RUNNING))
    expect(stream.entities.workspaces[WS_RUNNING]).toBeDefined()
    await ws.loadOne(WS_RUNNING)
    await settle()
    expect(ws.detail[WS_RUNNING]?.status).toBe('not_found')
    expect(stream.entities.workspaces[WS_RUNNING]).toBeUndefined()
    expect(deleteKey(WS_RUNNING) in stream.inFlight).toBe(false)
  })

  it('a snapshot requested before the request was accepted cannot resolve it', async () => {
    // WS_FAILED is failed — not building — both before a rebuild's receipt
    // and after its end. A body read before the 202 cannot tell the two
    // apart, so only one read after it may speak.
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { stream, ws } = await live(b)
    // A list request made before the rebuild is sent, whose body lands after
    // its 202: the order a slow GET and a fast POST can take.
    const early = stream.snapshotTag()
    const body = await api.get<WorkspaceList>('/api/workspaces')
    await ws.rebuild(WS_FAILED)
    stream.snapshot({ type: 'workspaces', at: early.at, view: body }, early.tick)
    expect(stream.entities.workspaces[WS_FAILED]?.state).toBe('failed')
    expect(rebuildKey(WS_FAILED) in stream.inFlight).toBe(true)
    // Control: the same body, read after the 202, does resolve it. (Here the
    // mock has not played the receipt, so the row still says failed — which
    // is exactly the ambiguity the tag exists to keep out of the early read.)
    await ws.loadList()
    expect(rebuildKey(WS_FAILED) in stream.inFlight).toBe(false)
  })
})

describe('the event path and the snapshot path cannot disagree', () => {
  // Each script played one event at a time into a live stream: after every
  // event, "the settling event has arrived" (the mark is gone) must equal
  // "the entities show it over" — the one OVER predicate asked both ways.
  // From the receipt on: before it, the entities still show the state the
  // action began from, which is why a snapshot must postdate the 202.
  const scripts: Array<{
    name: string; id: string; key: string; over: (o: Outcome) => boolean
    setup?: (b: MockBackend) => void; run: (ws: ReturnType<typeof useWorkspacesStore>) => Promise<void>
  }> = [
    { name: 'stop', id: WS_RUNNING, key: stopKey(WS_RUNNING), over: OVER.stop, run: (ws) => ws.stop(WS_RUNNING) },
    {
      name: 'failed stop', id: WS_RUNNING, key: stopKey(WS_RUNNING), over: OVER.stop, run: (ws) => ws.stop(WS_RUNNING),
      setup: (b) => { b.failAction = 'container' },
    },
    { name: 'rebuild', id: WS_RUNNING, key: rebuildKey(WS_RUNNING), over: OVER.build, run: (ws) => ws.rebuild(WS_RUNNING) },
    {
      name: 'failed rebuild', id: WS_RUNNING, key: rebuildKey(WS_RUNNING), over: OVER.build,
      run: (ws) => ws.rebuild(WS_RUNNING), setup: (b) => { b.failNext = 'up' },
    },
    { name: 'start', id: WS_FAILED, key: startKey(WS_FAILED), over: OVER.build, run: (ws) => ws.start(WS_FAILED) },
    {
      name: 'delete', id: WS_RUNNING, key: deleteKey(WS_RUNNING), over: OVER.delete,
      run: (ws) => ws.remove(WS_RUNNING, 'krelinga/drydock'),
    },
    {
      name: 'stuck delete', id: WS_RUNNING, key: deleteKey(WS_RUNNING), over: OVER.delete,
      run: (ws) => ws.remove(WS_RUNNING, 'krelinga/drydock'), setup: (b) => { b.failAction = 'containers' },
    },
  ]
  for (const s of scripts) {
    it(s.name, async () => {
      const b = freshBackend({ signedIn: true, scriptMode: 'manual', capacity: 10 })
      s.setup?.(b)
      const { stream, ws } = await live(b)
      await s.run(ws)
      playScript(b, s.id, 1)
      let steps = 0
      let ended = false
      while ((b.scripts[s.id]?.length ?? 0) > 0 && !ended) {
        const settled = !(s.key in stream.inFlight)
        expect(settled, `after ${steps} events`).toBe(s.over(entityOutcome(stream.entities, s.id)))
        playScript(b, s.id, 1)
        steps++
        ended = !(s.key in stream.inFlight)
      }
      // Positive control: the script did end the action, both ways.
      expect(steps).toBeGreaterThan(0)
      expect(s.key in stream.inFlight).toBe(false)
      expect(s.over(entityOutcome(stream.entities, s.id))).toBe(true)
    })
  }
})
