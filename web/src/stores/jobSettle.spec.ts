// A press ends with its job (design §5, frontend §4.2): every job the server
// runs for a workspace ends with one `workspace.job` event of its kind, and
// that ends the mark whichever path the job took — even one whose other
// events imply nothing, which is how #85's and #86's buttons span forever.
// The OVER predicates still settle a mark (and the snapshot backstop still
// needs them); the job's end is the clause that needs no error path to
// remember anything.

import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import type { StreamEvent } from '../api/types'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, settle, useMockApi } from '../test/setup'
import { playScript, WS_FAILED, WS_RUNNING, type MockBackend } from '../mocks/backend'
import { useStreamStore } from './stream'
import { deleteKey, rebuildKey, sessionKey, settlesJob, startKey, stopKey, useWorkspacesStore } from './workspaces'

useMockApi()

beforeEach(() => {
  setActivePinia(createPinia())
})

afterEach(() => {
  useStreamStore().disconnect()
})

/** A workspace.job event as the server writes it, with the next id unless one is given. */
function jobEnd(id: string, kind: string, outcome = 'failed', evId?: number): StreamEvent {
  const stream = useStreamStore()
  return {
    id: evId ?? stream.lastEventId + 1, level: 'warn', kind: 'workspace.job', message: `The ${kind} job ${outcome}.`,
    workspace_id: id, data: { kind, outcome }, at: new Date().toISOString(),
  }
}

describe('a workspace.job ends the press that started its job', () => {
  it('settles a restart whose server path wrote nothing but the end (#86\'s shape), and nothing else does', async () => {
    freshBackend({ signedIn: true, scriptMode: 'manual', supervisor: true })
    const stream = useStreamStore()
    const ws = useWorkspacesStore()
    // Something already on the stream, so "older than the mark" can be said.
    stream.receive({ id: 40, level: 'info', kind: 'token.issued', message: 'x', at: new Date().toISOString() })
    await ws.restartSession(WS_RUNNING)
    expect(sessionKey(WS_RUNNING) in stream.inFlight).toBe(true)

    // Another kind's end, for this workspace: not this press.
    stream.receive(jobEnd(WS_RUNNING, 'stop'))
    // This kind's end, for another workspace: not this press either.
    stream.receive(jobEnd(WS_FAILED, 'supervisor'))
    // A late one, from before the mark: an earlier press's job.
    stream.receive(jobEnd(WS_RUNNING, 'supervisor', 'ok', 39))
    expect(sessionKey(WS_RUNNING) in stream.inFlight).toBe(true)

    // The job's own end — no supervisor.state, no workspace.state — settles it.
    stream.receive(jobEnd(WS_RUNNING, 'supervisor', 'failed'))
    expect(sessionKey(WS_RUNNING) in stream.inFlight).toBe(false)
    // And it changed no entity: the workspace is as it was, the event in its feed.
    const w = stream.entities.workspaces[WS_RUNNING]
    expect(w === undefined || w.state === null || w.state === 'running').toBe(true)
    expect(stream.entities.feeds[WS_RUNNING]?.some((e) => e.kind === 'workspace.job')).toBe(true)
  })

  it('settles each workspace action by its own kind', async () => {
    freshBackend({ signedIn: true, scriptMode: 'manual' })
    const stream = useStreamStore()
    const ws = useWorkspacesStore()
    const cases: Array<[() => Promise<void>, string, string, string]> = [
      [() => ws.stop(WS_RUNNING), stopKey(WS_RUNNING), WS_RUNNING, 'stop'],
      [() => ws.start(WS_FAILED), startKey(WS_FAILED), WS_FAILED, 'start'],
    ]
    for (const [press, key, id, kind] of cases) {
      await press()
      expect(key in stream.inFlight).toBe(true)
      stream.receive(jobEnd(id, kind === 'stop' ? 'start' : 'stop')) // the other one's
      expect(key in stream.inFlight).toBe(true)
      stream.receive(jobEnd(id, kind, 'cancelled'))
      expect(key in stream.inFlight).toBe(false)
    }
  })

  it('settlesJob is exactly: this kind, this workspace, newer than the mark', () => {
    const ends = settlesJob(WS_RUNNING, 'rebuild', 10)
    const ev = (over: Partial<StreamEvent>): StreamEvent => ({
      id: 11, level: 'info', kind: 'workspace.job', message: '', workspace_id: WS_RUNNING,
      data: { kind: 'rebuild', outcome: 'ok' }, at: '', ...over,
    })
    expect(ends(ev({}))).toBe(true)
    expect(ends(ev({ id: 10 }))).toBe(false)
    expect(ends(ev({ workspace_id: WS_FAILED }))).toBe(false)
    expect(ends(ev({ data: { kind: 'start', outcome: 'ok' } }))).toBe(false)
    expect(ends(ev({ kind: 'workspace.state', data: { kind: 'rebuild', state: 'running' } }))).toBe(false)
  })
})

describe('the mock ends every job with one workspace.job, as the server does', () => {
  async function live(b: MockBackend) {
    const stream = useStreamStore()
    const ws = useWorkspacesStore()
    stream.connect()
    FakeEventSource.latest().open().pipe(b)
    await ws.loadList()
    return { stream, ws }
  }
  const ends = (b: MockBackend, id: string, since: number) =>
    b.events.filter((e) => e.kind === 'workspace.job' && e.workspace_id === id && e.id > since).map((e) => e.data)

  it('a restart whose stop fails ends failed, and its press settles', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual', supervisor: true })
    b.supervisorStopFails = 'stop_failed'
    const { stream, ws } = await live(b)
    const mark = b.events.length
    await ws.restartSession(WS_RUNNING)
    playScript(b, WS_RUNNING)
    await settle()
    expect(ends(b, WS_RUNNING, mark)).toEqual([{ kind: 'supervisor', outcome: 'failed' }])
    expect(sessionKey(WS_RUNNING) in stream.inFlight).toBe(false)
  })

  it('a rebuild cut off by a delete ends cancelled, after the move to deleting and before the delete\'s first sub-step', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { stream, ws } = await live(b)
    const mark = b.events[b.events.length - 1]!.id
    await ws.rebuild(WS_RUNNING)
    playScript(b, WS_RUNNING, 2)
    await ws.remove(WS_RUNNING, 'krelinga/drydock')
    playScript(b, WS_RUNNING, 1) // the move to deleting
    await settle()
    expect(ends(b, WS_RUNNING, mark)).toEqual([])
    playScript(b, WS_RUNNING, 1) // the cut rebuild's end
    await settle()
    expect(ends(b, WS_RUNNING, mark)).toEqual([{ kind: 'rebuild', outcome: 'cancelled' }])
    expect(rebuildKey(WS_RUNNING) in stream.inFlight).toBe(false)
    playScript(b, WS_RUNNING)
    await settle()
    expect(ends(b, WS_RUNNING, mark)).toEqual([{ kind: 'rebuild', outcome: 'cancelled' }, { kind: 'delete', outcome: 'ok' }])
    const evs = b.events.filter((e) => e.workspace_id === WS_RUNNING && e.id > mark)
    const kinds = evs.map((e) => e.kind)
    const deleting = evs.findIndex((e) => e.kind === 'workspace.state' && e.data?.state === 'deleting')
    expect(deleting).toBeGreaterThanOrEqual(0)
    expect(deleting).toBeLessThan(kinds.indexOf('workspace.job'))
    expect(kinds.indexOf('workspace.job')).toBeLessThan(kinds.indexOf('workspace.action'))
    expect(kinds[kinds.length - 1]).toBe('workspace.job')
    expect(deleteKey(WS_RUNNING) in stream.inFlight).toBe(false)
  })
})
