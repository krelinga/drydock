// The reducer (frontend §10: "the single most valuable unit in the frontend").
// Plain functions, recorded sequences in, entity maps out. Every negative
// case keeps a positive control in the same test, so a reducer that ignored
// everything could not pass.

import { describe, expect, it } from 'vitest'
import type { StreamEvent } from '../api/types'
import {
  FEED_LIMIT, currentStep, deleteStuck, emptyEntities, failedStep, hasStubs, liveAction, reduce, reduceAll, runSteps,
  stopFailed, workspaceForRepo,
  type Action, type Entities,
} from './reducer'
import {
  CLONE_FAILS_AT_UP, CLONE_OK, DELETE, ORPHAN, RECONCILE, REFRESH_FAILED, REFRESHED, RESTART_FAILS_EARLY,
  START_AFTER_FAIL, WS, WS2, catalogBody, detailBody, listBody, stateEvent, step, stepEvent, tokenIssued, ws2View, wsView,
  DELETE_OK, DELETE_RESUMED, DELETE_STUCK, STOP_FAILED_DETAIL, STOP_FAILS, STOP_OK, STOP_RETRIED, STUCK_DETAIL,
  actionEvent, listWithCap, supEvent,
} from './reducer.fixtures'

const events = (evs: StreamEvent[]): Action[] => evs.map((event) => ({ type: 'event', event }))
const play = (evs: StreamEvent[], start: Entities = emptyEntities()) => reduceAll(start, events(evs))

/** Deterministic shuffle, so a failure reproduces. */
function shuffled<T>(xs: T[], seed: number): T[] {
  const out = [...xs]
  let s = seed
  for (let i = out.length - 1; i > 0; i--) {
    s = (s * 1103515245 + 12345) & 0x7fffffff
    const j = s % (i + 1)
    ;[out[i], out[j]] = [out[j]!, out[i]!]
  }
  return out
}

describe('a recorded clone', () => {
  it('ends running, joined to its repository, and remembers the last step', () => {
    const e = play(CLONE_OK)
    expect(e.workspaces[WS]).toMatchObject({
      id: WS, repositoryId: 1, branch: 'main', state: 'running', stateAt: 13,
      step: { name: 'up', status: 'done', detail: null }, stepAt: 12,
    })
    expect(e.lastEventId).toBe(13)
  })

  it('passes through every state in order, one event at a time', () => {
    const seen: Array<string | null> = []
    let e = emptyEntities()
    for (const a of events(CLONE_OK)) {
      e = reduce(e, a)
      const s = e.workspaces[WS]?.state ?? null
      if (seen[seen.length - 1] !== s) seen.push(s)
    }
    expect(seen).toEqual(['pending', 'cloning', 'building', 'running'])
  })

  it('a failed build names the step that failed', () => {
    const e = play(CLONE_FAILS_AT_UP)
    expect(e.workspaces[WS2]).toMatchObject({
      state: 'failed', detail: 'The container start step failed.',
      step: { name: 'up', status: 'failed', detail: 'The container start step failed.' },
    })
  })

  it('never mutates its input', () => {
    const before = play(CLONE_OK.slice(0, 5))
    const frozen = JSON.stringify(before)
    play(CLONE_OK.slice(5), before)
    reduce(before, { type: 'resync', id: 99 })
    reduce(before, { type: 'snapshot', at: 99, view: catalogBody() })
    reduce(before, { type: 'workspaces', at: 99, view: listBody() })
    reduce(before, { type: 'workspace', at: 99, view: detailBody() })
    expect(JSON.stringify(before)).toBe(frozen)
  })
})

describe('out-of-order ids', () => {
  it('a late, older state event does not roll a workspace backwards', () => {
    // Control: in order, the older event does apply.
    expect(play([stateEvent(4, WS, 'cloning', { from: 'pending' })]).workspaces[WS]?.state).toBe('cloning')

    const e = play([
      CLONE_OK[0]!, // 1: pending
      stateEvent(9, WS, 'building', { from: 'cloning' }),
      stateEvent(4, WS, 'cloning', { from: 'pending' }), // arrives late
    ])
    expect(e.workspaces[WS]?.state).toBe('building')
    expect(e.workspaces[WS]?.stateAt).toBe(9)
    expect(e.lastEventId).toBe(9)
  })

  it('state and step are versioned apart, so a late step still lands', () => {
    // 12 (step up done) arrives after 13 (running): it is older than the
    // state but newer than any step seen, so it applies to the step only.
    const e = play([...CLONE_OK.slice(0, 10), CLONE_OK[12]!, CLONE_OK[11]!])
    expect(e.workspaces[WS]).toMatchObject({ state: 'running', step: { name: 'up', status: 'done' } })
  })

  it('any arrival order of a whole sequence converges on the in-order result', () => {
    const all = [...CLONE_OK, ...CLONE_FAILS_AT_UP]
    const inOrder = play(all)
    for (const seed of [1, 2, 3, 42, 1337]) {
      const e = play(shuffled(all, seed))
      expect(e.workspaces[WS]?.state).toBe(inOrder.workspaces[WS]?.state)
      expect(e.workspaces[WS2]?.state).toBe('failed')
      expect(e.workspaces[WS2]?.step).toEqual(inOrder.workspaces[WS2]?.step)
      expect(e.lastEventId).toBe(25)
    }
  })
})

describe('a replayed gap', () => {
  it('is idempotent: overlap with what was applied changes nothing', () => {
    const once = play(CLONE_OK)
    // The browser reconnected with Last-Event-ID 8, but the server's replay
    // (or a hard retry with no Last-Event-ID followed by events) overlaps.
    const twice = play(CLONE_OK.slice(5), once)
    expect(twice.workspaces).toEqual(once.workspaces)
    expect(twice.lastEventId).toBe(once.lastEventId)
  })

  it('a duplicate of an applied event is a no-op, by identity', () => {
    const e = play(CLONE_OK)
    const again = reduce(e, { type: 'event', event: CLONE_OK[12]! })
    expect(again.workspaces).toBe(e.workspaces)
    // Control: a genuinely new event does produce a new map.
    const moved = reduce(e, { type: 'event', event: stateEvent(14, WS, 'stopped', { from: 'running' }) })
    expect(moved.workspaces).not.toBe(e.workspaces)
    expect(moved.workspaces[WS]?.state).toBe('stopped')
  })

  it('a gap closed by replay ends where live delivery would have', () => {
    const live = play(CLONE_OK)
    const dropped = play(CLONE_OK.slice(0, 4)) // connection lost after id 4
    const replayed = play(CLONE_OK.slice(4), dropped)
    expect(replayed).toEqual(live)
  })
})

describe('resync', () => {
  it('marks the entities stale without discarding them, and advances the position', () => {
    const e = reduce(play(CLONE_OK), { type: 'resync', id: 500 })
    expect(e.staleSince).toBe(500)
    expect(e.lastEventId).toBe(500)
    // Stale-but-labelled beats blank (§4.3): the workspace is still there.
    expect(e.workspaces[WS]?.state).toBe('running')
  })

  it('is cleared by a snapshot taken at or after it, and not by one from before', () => {
    const stale = reduce(play(CLONE_OK), { type: 'resync', id: 500 })
    const early = reduce(stale, { type: 'snapshot', at: 13, view: catalogBody() })
    expect(early.staleSince).toBe(500)
    const late = reduce(stale, { type: 'snapshot', at: 500, view: catalogBody() })
    expect(late.staleSince).toBeNull()
  })

  it('on an empty log (latest id 0) is still cleared by the next snapshot', () => {
    const stale = reduce(emptyEntities(), { type: 'resync', id: 0 })
    expect(stale.staleSince).toBe(0)
    expect(reduce(stale, { type: 'snapshot', at: 0, view: catalogBody() }).staleSince).toBeNull()
  })
})

describe('an event for an unknown workspace', () => {
  it('a state event with its create data is a whole workspace, no refetch owed', () => {
    const e = play([CLONE_OK[0]!])
    expect(e.workspaces[WS]).toMatchObject({ state: 'pending', repositoryId: 1, branch: 'main' })
    expect(hasStubs(e)).toBe(false)
  })

  it('a step or a plain move for a workspace never seen becomes a stub that asks for a refetch', () => {
    const step = play([CLONE_OK[4]!]) // workspace.step, clone started
    expect(step.workspaces[WS]).toMatchObject({ state: null, repositoryId: null, step: { name: 'clone' } })
    expect(hasStubs(step)).toBe(true)

    const move = play([stateEvent(9, WS, 'building', { from: 'cloning' })])
    expect(move.workspaces[WS]).toMatchObject({ state: 'building', repositoryId: null })
    expect(hasStubs(move)).toBe(true)
  })

  it('the refetch places the stub, and a stub the workspace list does not know is dropped', () => {
    const stub = play([stateEvent(9, WS, 'building', { from: 'cloning' }), stateEvent(10, WS2, 'cloning')])
    const placed = reduce(stub, { type: 'workspaces', at: 10, view: listBody(wsView({ state: 'building' })) })
    expect(placed.workspaces[WS]).toMatchObject({ state: 'building', repositoryId: 1, fullName: 'krelinga/drydock' })
    // WS2 has no row as of 10, so it is dropped rather than asking for
    // refetches forever.
    expect(placed.workspaces[WS2]).toBeUndefined()
    expect(hasStubs(placed)).toBe(false)
  })

  it('the catalog places a stub too, but its silence drops nothing', () => {
    const stub = play([stateEvent(9, WS, 'building', { from: 'cloning' }), stateEvent(10, WS2, 'cloning')])
    const placed = reduce(stub, {
      type: 'snapshot', at: 10,
      view: catalogBody({ repos: [{ ...catalogBody().repos[0]!, workspace: { id: WS, state: 'building' } }] }),
    })
    expect(placed.workspaces[WS]).toMatchObject({ state: 'building', repositoryId: 1 })
    // The catalog joins only each repo's newest workspace, so not mentioning
    // WS2 is not evidence it is gone: it stays, still a stub.
    expect(placed.workspaces[WS2]).toMatchObject({ state: 'cloning', repositoryId: null })
    expect(hasStubs(placed)).toBe(true)
  })

  it('the orphan adopted at boot arrives whole; workspace.adopted only marks a known one', () => {
    const e = play([...CLONE_OK, ...RECONCILE])
    expect(e.workspaces[ORPHAN]).toMatchObject({
      state: 'running', adopted: true, repositoryId: 3,
      detail: 'Found with no record; adopted from its labels.',
    })
    expect(e.workspaces[WS]).toMatchObject({ state: 'running', adopted: true })
    expect(e.lastEventId).toBe(43)
  })
})

describe('deletion', () => {
  it('removes the workspace, and nothing late can bring it back', () => {
    const e = play([...CLONE_OK, ...DELETE])
    expect(e.workspaces[WS]).toBeUndefined()
    expect(e.gone[WS]).toBe(31)
    // Control: right before the gone, it was there and deleting.
    expect(play([...CLONE_OK, DELETE[0]!]).workspaces[WS]?.state).toBe('deleting')

    // A late state event, a step, and a snapshot taken before the delete.
    const late = play([stateEvent(29, WS, 'running'), CLONE_OK[10]!], e)
    expect(late.workspaces[WS]).toBeUndefined()
    const snap = reduce(e, { type: 'snapshot', at: 25, view: catalogBody() })
    expect(snap.workspaces[WS]).toBeUndefined()
    // Control: the same snapshot does place a workspace that was never deleted.
    expect(reduce(emptyEntities(), { type: 'snapshot', at: 25, view: catalogBody() }).workspaces[WS]?.state).toBe('running')
  })
})

describe('kinds that change no entity', () => {
  it('token, container and system events advance the position only', () => {
    const before = play(CLONE_OK.slice(0, 10))
    const after = play([CLONE_OK[10]!, RECONCILE[2]!, RECONCILE[3]!].map((e, i) => ({ ...e, id: 100 + i })), before)
    expect(after.workspaces).toBe(before.workspaces)
    expect(after.lastEventId).toBe(102)
  })

  it('an unknown kind is not an error, and an unknown state is not applied', () => {
    const e = play(CLONE_OK)
    const x = play([
      { id: 50, kind: 'supervisor.state', workspace_id: WS, level: 'info', message: '', at: '', data: { state: 'serving' } },
      stateEvent(51, WS, 'exploded'),
    ], e)
    expect(x.workspaces[WS]?.state).toBe('running')
    expect(x.lastEventId).toBe(51)
    // Control: a real state at the next id does apply.
    expect(play([stateEvent(52, WS, 'stopped')], x).workspaces[WS]?.state).toBe('stopped')
  })

  it('an event without a usable id is ignored entirely', () => {
    const e = play(CLONE_OK)
    const bad = play([{ ...stateEvent(0, WS, 'stopped') }, { ...stateEvent(1.5, WS, 'stopped') }], e)
    expect(bad).toBe(e)
  })
})

describe('repo.* outcomes', () => {
  it('records the newest refresh outcome, by id, with the counts or the sentence', () => {
    const ok = play([REFRESHED(60, 7)])
    expect(ok.lastRefresh).toMatchObject({ ok: true, count: 7, added: 1, removed: 0, eventId: 60 })
    const failed = play([REFRESH_FAILED(61)], ok)
    expect(failed.lastRefresh).toMatchObject({
      ok: false, count: null, message: 'Could not refresh the repository list from GitHub: Bad credentials',
    })
    // A replayed older success does not hide the newer failure.
    expect(play([REFRESHED(60, 7)], failed).lastRefresh?.ok).toBe(false)
  })
})

describe('snapshots', () => {
  it('load repositories in server order, with installations and refreshed_at', () => {
    const e = reduce(emptyEntities(), { type: 'snapshot', at: 0, view: catalogBody() })
    expect(e.catalogLoaded).toBe(true)
    expect(e.repoOrder).toEqual([1, 2])
    expect(e.repos[2]?.hasDevcontainer).toBeNull()
    expect(e.installations[101]?.settings_url).toContain('/installations/101')
    expect(workspaceForRepo(e, 1)?.state).toBe('running')
    expect(workspaceForRepo(e, 2)).toBeNull()
  })

  it('keep an event newer than the snapshot position over the snapshot', () => {
    // The fetch was asked at 13 (running); a stop at 14 arrived before the
    // body. The body may predate the stop, so the event wins.
    const e = play([...CLONE_OK, stateEvent(14, WS, 'stopped', { from: 'running' })])
    const merged = reduce(e, { type: 'snapshot', at: 13, view: catalogBody() })
    expect(merged.workspaces[WS]?.state).toBe('stopped')
    // Control: a snapshot asked after the stop is believed.
    const later = reduce(e, { type: 'snapshot', at: 14, view: catalogBody() })
    expect(later.workspaces[WS]?.state).toBe('running')
  })

  it('beat a straggling event from before the snapshot', () => {
    const snap = reduce(emptyEntities(), { type: 'snapshot', at: 13, view: catalogBody() })
    const e = play([stateEvent(9, WS, 'building')], snap)
    expect(e.workspaces[WS]?.state).toBe('running')
    // Control: an event after the snapshot position applies.
    expect(play([stateEvent(14, WS, 'stopped')], snap).workspaces[WS]?.state).toBe('stopped')
  })

  it('replace repositories outright: one gone from the body is gone from the list', () => {
    const first = reduce(emptyEntities(), { type: 'snapshot', at: 0, view: catalogBody() })
    const second = reduce(first, { type: 'snapshot', at: 0, view: catalogBody({ repos: [catalogBody().repos[1]!] }) })
    expect(second.repoOrder).toEqual([2])
    expect(second.repos[1]).toBeUndefined()
  })

  it('a new workspace for a repo is joined to it from the stream alone', () => {
    const snap = reduce(emptyEntities(), { type: 'snapshot', at: 13, view: catalogBody() })
    expect(workspaceForRepo(snap, 2)).toBeNull()
    const e = play(CLONE_FAILS_AT_UP.map((x) => ({ ...x, id: x.id + 100 })), snap)
    expect(workspaceForRepo(e, 2)).toMatchObject({ id: WS2, state: 'failed' })
  })
})

describe('the step timeline', () => {
  it('keeps each step\'s latest status, and the progress line the newest step', () => {
    const e = play(CLONE_OK)
    const w = e.workspaces[WS]!
    expect(Object.keys(w.steps)).toEqual(['allocate', 'clone', 'resolve_config', 'up'])
    expect(w.steps.up).toMatchObject({ status: 'done', eventId: 12, at: CLONE_OK[11]!.at })
    expect(currentStep(w)).toEqual({ name: 'up', status: 'done', detail: null })
  })

  it('a late event for an earlier step lands in its own row and leaves the progress line alone', () => {
    // 6 (clone done) arrives after 10 (up started).
    const e = play([...CLONE_OK.slice(0, 5), ...CLONE_OK.slice(6, 10), CLONE_OK[5]!])
    const w = e.workspaces[WS]!
    expect(w.steps.clone).toMatchObject({ status: 'done', eventId: 6 })
    expect(w.step).toEqual({ name: 'up', status: 'started', detail: null })
    // Control: an older event for that same row does not overwrite it.
    const back = play([CLONE_OK[4]!], e) // 5: clone started
    expect(back.workspaces[WS]!.steps.clone?.status).toBe('done')
  })

  it('names the failed step, and a start that reruns it replaces the failure', () => {
    const failed = play(CLONE_FAILS_AT_UP)
    expect(failedStep(failed.workspaces[WS2]!)).toBe('up')
    expect(failed.workspaces[WS2]!.steps.up).toMatchObject({ status: 'failed', detail: 'The container start step failed.' })

    const started = play(START_AFTER_FAIL, failed)
    expect(started.workspaces[WS2]).toMatchObject({ state: 'running', detail: null })
    expect(started.workspaces[WS2]!.steps.up).toMatchObject({ status: 'done', detail: null, eventId: 52 })
    expect(failedStep(started.workspaces[WS2]!)).toBeNull()
  })

  it('a create starts a fresh timeline; a start does not', () => {
    const e = play([...CLONE_FAILS_AT_UP, ...START_AFTER_FAIL.slice(0, 1)])
    // Control: the move to building kept the previous run's rows.
    expect(e.workspaces[WS2]!.steps.up?.status).toBe('failed')
    // A stub that saw a step before its create: the create (lower id) keeps it.
    const early = play([stepEvent(2, WS, 'allocate', 'started'), CLONE_OK[0]!])
    expect(early.workspaces[WS]!.steps.allocate?.status).toBe('started')
    // But a row written before a create is not this workspace's history.
    const before = play([stepEvent(5, WS, 'up', 'failed'), stateEvent(7, WS, 'pending', { repository_id: 1, branch: 'main' })])
    expect(before.workspaces[WS]!.steps).toEqual({})
    expect(before.workspaces[WS]!.step).toBeNull()
  })
})

describe('the current run (frontend §6.1, steps after a start)', () => {
  it('leaves out a step an earlier run reached and this one has not, from events', () => {
    const before = play([...CLONE_OK, ...RESTART_FAILS_EARLY.slice(0, 2)])
    // Control: until the start writes a step, the last run's timeline stands.
    expect(Object.keys(runSteps(before.workspaces[WS]!))).toEqual(['allocate', 'clone', 'resolve_config', 'up'])

    const e = play(RESTART_FAILS_EARLY.slice(2), before)
    const w = e.workspaces[WS]!
    // The raw rows still hold `up` from the first run — what the server reports…
    expect(w.steps.up).toMatchObject({ status: 'done', eventId: 12 })
    // …but it is not this run's: resolve_config, before it in run order, was written after it.
    expect(Object.keys(runSteps(w))).toEqual(['allocate', 'clone', 'resolve_config'])
    expect(runSteps(w).resolve_config).toMatchObject({ status: 'failed' })
    expect(failedStep(w)).toBe('resolve_config')
  })

  it('does the same from a snapshot alone, where every row carries one event id', () => {
    const body = wsView({
      state: 'failed', state_detail: 'Could not read the dev container configuration.',
      steps: {
        allocate: step('done', 3), clone: step('done', 6), resolve_config: step('failed', 63, 'Could not read…'),
        up: step('done', 12), verify: step('failed', 14, 'an older run'),
      },
    })
    const w = reduce(emptyEntities(), { type: 'workspaces', at: 70, view: listBody(body) }).workspaces[WS]!
    expect(Object.keys(runSteps(w)).sort()).toEqual(['allocate', 'clone', 'resolve_config'])
    // Ids tie, so run order alone would have named verify, the last step with a row.
    expect(failedStep(w)).toBe('resolve_config')
    expect(currentStep(w)).toMatchObject({ name: 'resolve_config', status: 'failed' })
    // Control: the same rows with the first run's times intact are all one run.
    const whole = reduce(emptyEntities(), {
      type: 'workspaces', at: 70,
      view: listBody(wsView({ steps: { ...body.steps, resolve_config: step('done', 8) } })),
    }).workspaces[WS]!
    expect(Object.keys(runSteps(whole)).sort()).toEqual(['allocate', 'clone', 'resolve_config', 'up', 'verify'])
    expect(failedStep(whole)).toBe('verify')
  })

  it('keeps the prefix a start resumed after, and a completed run whole', () => {
    const e = play([...CLONE_FAILS_AT_UP, ...START_AFTER_FAIL])
    expect(Object.keys(runSteps(e.workspaces[WS2]!))).toEqual(['up'])
    const ok = play(CLONE_OK).workspaces[WS]!
    expect(runSteps(ok)).toBe(ok.steps)
  })
})

describe('a step 8 closed at boot (design §6, reconciliation)', () => {
  // Drydock died inside step 8: the row is running with step 8 started, and
  // the next boot fails the step with its sentence, then restarts the
  // session server. The workspace stays running throughout.
  const CLOSED = 'The session server step failed: Drydock stopped while this step was running.'
  const BOOT: StreamEvent[] = [
    stepEvent(20, WS, 'session_server', 'failed', CLOSED),
    supEvent(21, 'starting', 'launching', 'Starting the session server.'),
    supEvent(22, 'serving', 'connected', '', 'starting'),
  ]
  const DIED = [...CLONE_OK, stepEvent(14, WS, 'session_server', 'started')]

  it('fails the step on a running workspace, from events and from a snapshot', () => {
    // Control: before the boot, step 8 reads as running.
    const before = play(DIED).workspaces[WS]!
    expect(currentStep(before)).toMatchObject({ name: 'session_server', status: 'started' })

    const w = play(BOOT, play(DIED)).workspaces[WS]!
    expect(w.state).toBe('running')
    expect(runSteps(w).session_server).toMatchObject({ status: 'failed', detail: CLOSED })
    expect(currentStep(w)).toMatchObject({ name: 'session_server', status: 'failed', detail: CLOSED })
    expect(w.supervisor).toMatchObject({ state: 'serving' })
    // The earlier steps are still this run's: the close is the run's last row.
    expect(Object.keys(runSteps(w))).toContain('up')

    const snap = reduce(emptyEntities(), {
      type: 'workspaces', at: 22,
      view: listBody(wsView({ steps: { ...wsView().steps, session_server: step('failed', 20, CLOSED) } })),
    }).workspaces[WS]!
    expect(snap.state).toBe('running')
    expect(runSteps(snap).session_server).toMatchObject({ status: 'failed', detail: CLOSED })
  })

  it('a later run’s step 8 replaces the closed one', () => {
    const e = play([
      ...BOOT,
      stateEvent(30, WS, 'building', { from: 'running' }),
      stepEvent(31, WS, 'resolve_config', 'started'),
      stepEvent(32, WS, 'resolve_config', 'done'),
      stateEvent(33, WS, 'running', { from: 'building' }),
      stepEvent(34, WS, 'session_server', 'started'),
      stepEvent(35, WS, 'session_server', 'done'),
    ], play(DIED))
    expect(runSteps(e.workspaces[WS]!).session_server).toMatchObject({ status: 'done', eventId: 35 })
  })
})

describe('the container id', () => {
  it('arrives on the move to running, with no refetch', () => {
    expect(play(CLONE_OK).workspaces[WS]).toMatchObject({ containerId: 'c0ffee0123456789', containerAt: 13 })
    // Control: a step short of running, there is none.
    expect(play(CLONE_OK.slice(0, 12)).workspaces[WS]!.containerId).toBeNull()
  })

  it('is versioned on its own: a late older event and an older snapshot do not replace it', () => {
    const e = play([...CLONE_OK, stateEvent(20, WS, 'running', { from: 'building', container_id: 'newer' })])
    const late = play([stateEvent(15, WS, 'running', { from: 'building', container_id: 'older' })], e)
    expect(late.workspaces[WS]!.containerId).toBe('newer')
    const snap = reduce(e, { type: 'workspaces', at: 18, view: listBody(wsView({ container_id: 'snapshot' })) })
    expect(snap.workspaces[WS]!.containerId).toBe('newer')
    // Control: a snapshot from after the event is believed.
    const fresh = reduce(e, { type: 'workspaces', at: 21, view: listBody(wsView({ container_id: 'snapshot' })) })
    expect(fresh.workspaces[WS]).toMatchObject({ containerId: 'snapshot', containerAt: 21 })
  })

  it('lands even when a snapshot already carried the state the event moves to', () => {
    // The list (at 12, state running from a racing read) beat the event that also names the container.
    const snap = reduce(play(CLONE_OK.slice(0, 12)), {
      type: 'workspaces', at: 12, view: listBody(wsView({ container_id: null })),
    })
    const e = play([CLONE_OK[12]!], snap)
    expect(e.workspaces[WS]!.containerId).toBe('c0ffee0123456789')
  })
})

describe('the workspace list snapshot', () => {
  it('places every workspace whole: name, steps, container, detail', () => {
    const e = reduce(emptyEntities(), { type: 'workspaces', at: 25, view: listBody() })
    expect(e.workspaces[WS]).toMatchObject({
      state: 'running', repositoryId: 1, fullName: 'krelinga/drydock', containerId: 'c0ffee0123456789',
      createdAt: CLONE_OK[0]!.at, stateAt: 25,
    })
    expect(e.workspaces[WS]!.steps.up).toMatchObject({ status: 'done', eventId: 25 })
    expect(e.workspaces[WS2]).toMatchObject({ state: 'failed', detail: 'The container start step failed.' })
    // From a snapshot alone, the card can still name the failed step.
    expect(failedStep(e.workspaces[WS2]!)).toBe('up')
    expect(currentStep(e.workspaces[WS]!)).toMatchObject({ name: 'up', status: 'done' })
    expect(hasStubs(e)).toBe(false)
    expect(e.lastEventId).toBe(25)
  })

  it('is the authority on existence: an unlisted workspace nothing newer touched is dropped', () => {
    const e = play([...CLONE_OK, ...CLONE_FAILS_AT_UP])
    const listed = reduce(e, { type: 'workspaces', at: 25, view: listBody(wsView()) })
    expect(listed.workspaces[WS2]).toBeUndefined()
    // Control: the same list asked before WS2's last event keeps it.
    const early = reduce(e, { type: 'workspaces', at: 24, view: listBody(wsView()) })
    expect(early.workspaces[WS2]?.state).toBe('failed')
  })

  it('keeps an event newer than the snapshot position, per field and per step', () => {
    const e = play([...CLONE_OK, stateEvent(14, WS, 'stopped', { from: 'running' }), stepEvent(15, WS, 'verify', 'started')])
    // The body was asked at 13 and carries verify's row from an earlier run.
    const body = wsView({ steps: { ...wsView().steps, verify: { status: 'failed', detail: 'old', at: CLONE_OK[0]!.at } } })
    const merged = reduce(e, { type: 'workspaces', at: 13, view: listBody(body) })
    expect(merged.workspaces[WS]?.state).toBe('stopped')
    expect(merged.workspaces[WS]?.steps.verify).toMatchObject({ status: 'started', eventId: 15 })
    // The body's other steps and facts still land.
    expect(merged.workspaces[WS]).toMatchObject({ fullName: 'krelinga/drydock', containerId: 'c0ffee0123456789' })
    // Control: a list asked after both is believed for both.
    const later = reduce(e, {
      type: 'workspaces', at: 15,
      view: listBody(wsView({ steps: { verify: { status: 'done', at: CLONE_OK[0]!.at } } })),
    })
    expect(later.workspaces[WS]?.state).toBe('running')
    expect(later.workspaces[WS]?.steps.verify?.status).toBe('done')
  })

  it('beats a straggling event from before it', () => {
    const snap = reduce(emptyEntities(), { type: 'workspaces', at: 25, view: listBody() })
    const e = play([stateEvent(9, WS, 'building'), stepEvent(10, WS, 'up', 'started')], snap)
    expect(e.workspaces[WS]?.state).toBe('running')
    expect(e.workspaces[WS]?.steps.up?.status).toBe('done')
    // Control: events after it apply.
    const after = play([stateEvent(26, WS, 'stopped')], snap)
    expect(after.workspaces[WS]?.state).toBe('stopped')
  })

  it('never resurrects a deleted workspace, and lists `deleting` until the gone', () => {
    const deleting = reduce(play([...CLONE_OK, DELETE[0]!]), {
      type: 'workspaces', at: 30, view: listBody(wsView({ state: 'deleting' })),
    })
    expect(deleting.workspaces[WS]?.state).toBe('deleting')
    const gone = play([DELETE[1]!], deleting)
    const stale = reduce(gone, { type: 'workspaces', at: 29, view: listBody(wsView()) })
    expect(stale.workspaces[WS]).toBeUndefined()
    // Control: the same body places WS on entities that never saw it deleted.
    expect(reduce(emptyEntities(), { type: 'workspaces', at: 29, view: listBody(wsView()) }).workspaces[WS]?.state).toBe('running')
  })

  it('clears a resync when taken at or after it', () => {
    const stale = reduce(play(CLONE_OK), { type: 'resync', id: 500 })
    expect(reduce(stale, { type: 'workspaces', at: 499, view: listBody() }).staleSince).toBe(500)
    expect(reduce(stale, { type: 'workspaces', at: 500, view: listBody() }).staleSince).toBeNull()
  })

  it('is idempotent: the same body twice changes nothing more', () => {
    const once = reduce(play(CLONE_OK), { type: 'workspaces', at: 13, view: listBody() })
    const twice = reduce(once, { type: 'workspaces', at: 13, view: listBody() })
    expect(twice).toEqual(once)
  })

  it('makes the join whole: an older workspace on a repo is listed beside the newest', () => {
    const older = { ...ws2View(), id: '01JA0000000000000000000009', repository_id: 1, state: 'running' as const }
    const e = reduce(emptyEntities(), { type: 'workspaces', at: 25, view: listBody(wsView(), older) })
    expect(Object.keys(e.workspaces).sort()).toEqual(['01JA0000000000000000000009', WS])
    // The catalog row still joins the newest.
    expect(workspaceForRepo(e, 1)?.id).toBe(WS)
  })
})

describe('the workspace detail snapshot and the feed', () => {
  it('every event naming a workspace joins its feed, newest first — token events too', () => {
    const e = play([...CLONE_OK.slice(0, 3), tokenIssued(4, WS), RECONCILE[3]!])
    expect(e.feeds[WS]!.map((x) => x.id)).toEqual([4, 3, 2, 1])
    // Control: an event naming no workspace joins no feed.
    expect(Object.keys(e.feeds)).toEqual([WS])
  })

  it('a replayed event is not doubled, and leaves the feed by identity', () => {
    const e = play(CLONE_OK)
    const again = play(CLONE_OK.slice(5), e)
    expect(again.feeds[WS]).toBe(e.feeds[WS])
    expect(e.feeds[WS]!.length).toBe(13)
  })

  it('merges the detail body\'s events with the stream\'s by id, never doubling', () => {
    // The stream delivered 10–13; the body (asked at 9) carries 1–9.
    const live = play(CLONE_OK.slice(9))
    const merged = reduce(live, { type: 'workspace', at: 9, view: detailBody({}, CLONE_OK.slice(0, 9)) })
    expect(merged.feeds[WS]!.map((x) => x.id)).toEqual([13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1])
    // And the body asked later, overlapping all of it, adds nothing.
    const again = reduce(merged, { type: 'workspace', at: 13, view: detailBody() })
    expect(again.feeds[WS]!.map((x) => x.id)).toEqual(merged.feeds[WS]!.map((x) => x.id))
  })

  it('caps the feed, keeping the newest', () => {
    const many = Array.from({ length: FEED_LIMIT + 10 }, (_, i) => tokenIssued(100 + i, WS))
    const e = play(many)
    expect(e.feeds[WS]!.length).toBe(FEED_LIMIT)
    expect(e.feeds[WS]![0]!.id).toBe(100 + FEED_LIMIT + 9)
    // An event older than everything kept falls off without changing the feed.
    expect(play([tokenIssued(50, WS)], e).feeds[WS]).toBe(e.feeds[WS])
  })

  it('places its workspace, but names one so drops none and clears no resync', () => {
    const base = reduce(play(CLONE_FAILS_AT_UP), { type: 'resync', id: 40 })
    const e = reduce(base, { type: 'workspace', at: 40, view: detailBody() })
    expect(e.workspaces[WS]).toMatchObject({ state: 'running', fullName: 'krelinga/drydock' })
    expect(e.workspaces[WS2]?.state).toBe('failed')
    expect(e.staleSince).toBe(40)
  })

  it('ignores a deleted workspace\'s body, and drops the feed with the workspace', () => {
    const e = play([...CLONE_OK, ...DELETE])
    expect(e.feeds[WS]).toBeUndefined()
    const late = reduce(e, { type: 'workspace', at: 31, view: detailBody() })
    expect(late.workspaces[WS]).toBeUndefined()
    expect(late.feeds[WS]).toBeUndefined()
    // Control: the deleting event itself was in the feed before the gone.
    expect(play([...CLONE_OK, DELETE[0]!]).feeds[WS]![0]!.id).toBe(30)
  })

  it('an adoption records the container id', () => {
    const e = play([...CLONE_OK, RECONCILE[1]!])
    expect(e.workspaces[WS]).toMatchObject({ adopted: true, containerId: 'c0ffee' })
  })
})

describe('workspace.action: a stop\'s and a delete\'s sub-steps (Phase 6)', () => {
  const ws = (e: Entities, id = WS) => e.workspaces[id]!
  const statuses = (e: Entities, id = WS) =>
    Object.fromEntries(Object.entries(ws(e, id).action?.steps ?? {}).map(([k, r]) => [k, r.status]))

  it('a stop is live while it runs, and over at the move to stopped', () => {
    const mid = play([...CLONE_OK, ...STOP_OK.slice(0, 3)])
    expect(ws(mid).state).toBe('running')
    expect(liveAction(ws(mid))).toMatchObject({ name: 'stop', last: { name: 'container', status: 'started' } })
    expect(statuses(mid)).toEqual({ session_server: 'done', container: 'started' })
    expect(ws(mid).action!.steps.session_server!.detail).toContain('Nothing to do yet')
    const done = play([...CLONE_OK, ...STOP_OK])
    expect(ws(done).state).toBe('stopped')
    expect(liveAction(ws(done))).toBeNull()
    // Control: the run itself is kept — over, not forgotten.
    expect(statuses(done)).toEqual({ session_server: 'done', container: 'done', broker_socket: 'done' })
  })

  it('a failed stop is ended by its annotation: running, the server\'s sentence, stopFailed', () => {
    const e = play([...CLONE_OK, ...STOP_FAILS])
    expect(ws(e)).toMatchObject({ state: 'running', detail: STOP_FAILED_DETAIL })
    expect(liveAction(ws(e))).toBeNull()
    expect(stopFailed(ws(e))).toBe(true)
    expect(ws(e).lastAction).toMatchObject({ name: 'stop', step: 'container', status: 'failed' })
    expect(statuses(e)).toEqual({ session_server: 'done', container: 'failed' })
    // Control: one event earlier — failed, not yet annotated — the run is live and stopFailed is not (yet) true.
    const before = play([...CLONE_OK, ...STOP_FAILS.slice(0, 4)])
    expect(liveAction(ws(before))?.last).toEqual({ name: 'container', status: 'failed', detail: "docker could not stop the workspace's container." })
    expect(stopFailed(ws(before))).toBe(false)
  })

  it('a stop asked again clears the failure as it starts, and is a new run', () => {
    const e = play([...CLONE_OK, ...STOP_FAILS])
    const cleared = play(STOP_RETRIED.slice(0, 1), e)
    expect(ws(cleared).detail).toBeNull()
    expect(stopFailed(ws(cleared))).toBe(false)
    const again = play(STOP_RETRIED.slice(1, 2), cleared)
    expect(statuses(again)).toEqual({ session_server: 'started' })
    expect(ws(again).action!.startId).toBe(86)
    expect(liveAction(ws(again))?.name).toBe('stop')
    // Control: without the clear, the annotation would still say it failed.
    expect(stopFailed(ws(e))).toBe(true)
  })

  it('a failed stop reads the same from the list alone, and only a failed stop does (§4.5 #15)', () => {
    const lastAction = { action: 'stop', step: 'container', status: 'failed' as const, detail: "docker could not stop the workspace's container.", at: '2026-10-04T12:01:23Z' }
    const cold = reduce(emptyEntities(), { type: 'workspaces', at: 84, view: listBody(wsView({ state_detail: STOP_FAILED_DETAIL, last_action: lastAction })) })
    expect(stopFailed(ws(cold))).toBe(true)
    expect(ws(cold).detail).toBe(STOP_FAILED_DETAIL)
    // Live and cold agree on everything the card reads.
    const live = play([...CLONE_OK, ...STOP_FAILS])
    expect([ws(cold).state, ws(cold).detail, ws(cold).lastAction?.step]).toEqual([ws(live).state, ws(live).detail, ws(live).lastAction?.step])
    // Controls: an adopted orphan's note is a detail on running with no stop behind it;
    // a failed stop whose annotation a retry cleared has no detail; a stop that worked is stopped.
    const adopted = reduce(emptyEntities(), { type: 'workspaces', at: 5, view: listBody(wsView({ state_detail: 'Found with no record; adopted from its labels.', last_action: null })) })
    expect(stopFailed(ws(adopted))).toBe(false)
    const retried = reduce(emptyEntities(), { type: 'workspaces', at: 85, view: listBody(wsView({ last_action: lastAction })) })
    expect(stopFailed(ws(retried))).toBe(false)
    const deleteFailed = reduce(emptyEntities(), { type: 'workspaces', at: 99, view: listBody(wsView({ state: 'deleting', state_detail: STUCK_DETAIL, last_action: { ...lastAction, action: 'delete', step: 'files' } })) })
    expect(stopFailed(ws(deleteFailed))).toBe(false)
    expect(deleteStuck(ws(deleteFailed))).toBe(true)
  })

  it('the last sub-step is versioned like any field', () => {
    const e = play([...CLONE_OK, ...STOP_FAILS])
    const done = { action: 'stop', step: 'broker_socket', status: 'done' as const, at: '2026-10-04T12:02:00Z' }
    // A list taken before the failure cannot replace it…
    const older = reduce(e, { type: 'workspaces', at: 82, view: listBody(wsView({ last_action: done })) })
    expect(ws(older).lastAction).toMatchObject({ step: 'container', status: 'failed', at: 83 })
    // …one taken after it can, and a body without the field changes nothing.
    const newer = reduce(e, { type: 'workspaces', at: 90, view: listBody(wsView({ last_action: done })) })
    expect(ws(newer).lastAction).toMatchObject({ step: 'broker_socket', status: 'done', at: 90 })
    const silent = reduce(e, { type: 'workspaces', at: 90, view: listBody(wsView()) })
    expect(ws(silent).lastAction).toBe(ws(e).lastAction)
    // And an event newer than a list replaces the list's.
    const after = play([actionEvent(91, WS, 'stop', 'session_server', 'started')], newer)
    expect(ws(after).lastAction).toMatchObject({ step: 'session_server', status: 'started', at: 91 })
  })

  it('a stuck delete keeps its failed sub-step and its annotation; nothing is live', () => {
    const e = play([...CLONE_OK, ...DELETE_STUCK])
    expect(ws(e)).toMatchObject({ state: 'deleting', detail: STUCK_DETAIL })
    expect(statuses(e)).toEqual({ session_server: 'done', containers: 'done', broker_socket: 'done', files: 'failed' })
    expect(liveAction(ws(e))).toBeNull()
    expect(deleteStuck(ws(e))).toBe(true)
    // Control: one event earlier — failed, not yet annotated — it is neither stuck nor without detail.
    const before = play([...CLONE_OK, ...DELETE_STUCK.slice(0, 9)])
    expect(deleteStuck(ws(before))).toBe(false)
    expect(liveAction(ws(before))?.last.status).toBe('failed')
  })

  it('a resumed delete clears the stuck note as it starts, and is a new run until the gone (§4.5 #16)', () => {
    // The clear alone — no sub-step of the resume yet, as a list-only page
    // or a page between the two events sees it — is already not stuck.
    const clearing = play([...CLONE_OK, ...DELETE_STUCK, ...DELETE_RESUMED.slice(0, 1)])
    expect(ws(clearing)).toMatchObject({ state: 'deleting', detail: null })
    expect(deleteStuck(ws(clearing))).toBe(false)
    const resuming = play([...CLONE_OK, ...DELETE_STUCK, ...DELETE_RESUMED.slice(0, 4)])
    expect(deleteStuck(ws(resuming))).toBe(false)
    expect(statuses(resuming)).toEqual({ session_server: 'done', containers: 'started' })
    // A list taken mid-resume, cold: nothing offers Delete again.
    const cold = reduce(emptyEntities(), { type: 'workspaces', at: 103, view: listBody(wsView({ state: 'deleting', state_detail: null })) })
    expect(deleteStuck(ws(cold))).toBe(false)
    // Control: a list taken while it was stuck does offer it.
    const stuckCold = reduce(emptyEntities(), { type: 'workspaces', at: 99, view: listBody(wsView({ state: 'deleting', state_detail: STUCK_DETAIL })) })
    expect(deleteStuck(ws(stuckCold))).toBe(true)
    const gone = play([...CLONE_OK, ...DELETE_STUCK, ...DELETE_RESUMED])
    expect(gone.workspaces[WS]).toBeUndefined()
    expect(gone.gone[WS]).toBe(109)
  })

  it('a delete in one go reaches workspace.gone, and a late sub-step cannot bring it back', () => {
    const e = play([...CLONE_FAILS_AT_UP, ...DELETE_OK])
    expect(e.workspaces[WS2]).toBeUndefined()
    expect(e.gone[WS2]).toBe(119)
    const late = play([DELETE_OK[7]!], e)
    expect(late.workspaces[WS2]).toBeUndefined()
    // Control: just before the gone, the whole run was there and live.
    const before = play([...CLONE_FAILS_AT_UP, ...DELETE_OK.slice(0, 9)])
    expect(liveAction(ws(before, WS2))?.name).toBe('delete')
    expect(Object.keys(ws(before, WS2).action!.steps)).toEqual(['session_server', 'containers', 'broker_socket', 'files'])
  })

  it('is versioned: replay is idempotent, a late sub-step lands in its own row, an older run\'s event is dropped', () => {
    const e = play([...CLONE_OK, ...DELETE_STUCK])
    expect(play(DELETE_STUCK, e)).toBe(e)
    // A late `started` for containers (older than its `done`) changes nothing.
    expect(ws(play([actionEvent(93, WS, 'delete', 'containers', 'started')], e)).action).toBe(ws(e).action)
    // Out of order within a run: the row takes the newer, the line the newest.
    const shuffledRun = play([...CLONE_OK, DELETE_STUCK[0]!, DELETE_STUCK[2]!, DELETE_STUCK[1]!])
    expect(ws(shuffledRun).action!.steps.session_server!.status).toBe('done')
    // An event from before the resume's start is not merged into the resume.
    const resumed = play(DELETE_RESUMED.slice(0, 3), e)
    expect(ws(play([actionEvent(97, WS, 'delete', 'files', 'started')], resumed)).action!.steps.files).toBeUndefined()
    // Control: the resume's own `files` does land.
    expect(ws(play([DELETE_RESUMED[7]!], resumed)).action!.steps.files!.status).toBe('started')
  })

  it('every action event joins the feed, and a malformed one changes no run', () => {
    const e = play([...CLONE_OK, ...STOP_OK.slice(0, 2)])
    expect(e.feeds[WS]![0]!.kind).toBe('workspace.action')
    const bad = play([{ ...actionEvent(72, WS, 'stop', 'container', 'started'), data: { action: 'stop', status: 'started' } }], e)
    expect(ws(bad).action).toBe(ws(e).action)
    expect(bad.feeds[WS]![0]!.id).toBe(72)
  })

  it('a detail body folds its action events in, so a reload still shows a stuck delete or a failed stop', () => {
    // A cold load of a stuck delete: only the snapshot, no live events.
    const stuckEvents = [...CLONE_OK, ...DELETE_STUCK]
    const view = detailBody({ state: 'deleting', state_detail: STUCK_DETAIL }, stuckEvents)
    const cold = reduce(emptyEntities(), { type: 'workspace', at: 99, view })
    expect(statuses(cold)).toEqual({ session_server: 'done', containers: 'done', broker_socket: 'done', files: 'failed' })
    expect(deleteStuck(ws(cold))).toBe(true)
    // A failed stop on reload: the body carries the annotation and the run it ended.
    const stopCold = reduce(emptyEntities(), { type: 'workspace', at: 84, view: detailBody({ state_detail: STOP_FAILED_DETAIL }, [...CLONE_OK, ...STOP_FAILS]) })
    expect(stopFailed(ws(stopCold))).toBe(true)
    expect(ws(stopCold).action?.steps.container?.status).toBe('failed')
    // Control: a finished stop on reload is not live — its move to stopped is in the body.
    const stoppedCold = reduce(emptyEntities(), { type: 'workspace', at: 76, view: detailBody({ state: 'stopped' }, [...CLONE_OK, ...STOP_OK]) })
    expect(liveAction(ws(stoppedCold))).toBeNull()
  })

  it('a list snapshot that moved the state past a half-seen run drops it', () => {
    const e = play([...CLONE_OK, ...STOP_OK.slice(0, 3)])
    // The rest of the stop fell in a gap; the list says stopped.
    const after = reduce(e, { type: 'workspaces', at: 80, view: listBody(wsView({ state: 'stopped' })) })
    expect(ws(after).action).toBeNull()
    // Control: a list that agrees with the state keeps the run live.
    const same = reduce(e, { type: 'workspaces', at: 80, view: listBody(wsView()) })
    expect(liveAction(ws(same))?.name).toBe('stop')
  })
})

describe('the cap (frontend §4.5 #17)', () => {
  it('is written by the workspace list, and only by one that carries it', () => {
    expect(emptyEntities().cap).toBeNull()
    const e = reduce(emptyEntities(), { type: 'workspaces', at: 13, view: listWithCap(10) })
    expect(e.cap).toBe(10)
    // A list from a server older than the field leaves it as it was.
    expect(reduce(e, { type: 'workspaces', at: 14, view: listBody() }).cap).toBe(10)
    // No cap is null, not a number.
    expect(reduce(e, { type: 'workspaces', at: 15, view: listWithCap(null) }).cap).toBeNull()
    // Control: no event writes it — not even a burst of moves.
    expect(play([...CLONE_OK, ...STOP_OK], e).cap).toBe(10)
  })
})
