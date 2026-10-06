// The supervisor and session halves of a workspace entity (Phase 5), written
// by supervisor.state and session.status events and the views' fields — and
// versioned like every field, so a replay is a no-op and a stale snapshot
// cannot roll a newer event back.

import { describe, expect, it } from 'vitest'
import type { StreamEvent } from '../api/types'
import { emptyEntities, reduce, reduceAll, type Action } from './reducer'
import { CLONE_OK, ENV, SERVE_OK, WS, listBody, sessionEvent, supEvent, wsView } from './reducer.fixtures'

const evs = (list: StreamEvent[]): Action[] => list.map((event) => ({ type: 'event' as const, event }))

describe('the supervisor entity', () => {
  it('is written by its events, and a replay changes nothing', () => {
    const once = reduceAll(emptyEntities(), evs([...CLONE_OK, ...SERVE_OK]))
    const w = once.workspaces[WS]!
    expect(w.supervisor).toMatchObject({ state: 'serving', reason: 'connected', restartCount: 0 })
    expect(w.session).toMatchObject({ environmentId: ENV, capacityUsed: 1, capacityTotal: 4, sessions: 1 })
    const twice = reduceAll(once, evs(SERVE_OK))
    expect(twice.workspaces[WS]).toBe(w)
  })

  it('a late, older event does not roll the state back', () => {
    const e = reduceAll(emptyEntities(), evs([...CLONE_OK, ...SERVE_OK, supEvent(30, 'degraded', 'budget_spent')]))
    const late = reduce(e, { type: 'event', event: supEvent(20, 'serving', 'connected') })
    expect(late.workspaces[WS]!.supervisor!.state).toBe('degraded')
  })

  it('a snapshot older than an applied event keeps the event; a newer one replaces it', () => {
    const e = reduceAll(emptyEntities(), evs([...CLONE_OK, ...SERVE_OK, supEvent(30, 'degraded', 'budget_spent')]))
    const sup = { state: 'serving' as const, reason: 'connected', restart_count: 0, at: '2026-10-04T12:00:00Z' }
    const older = reduce(e, { type: 'workspaces', at: 25, view: listBody(wsView({ supervisor: sup })) })
    expect(older.workspaces[WS]!.supervisor!.state).toBe('degraded')
    const newer = reduce(e, { type: 'workspaces', at: 40, view: listBody(wsView({ supervisor: sup })) })
    expect(newer.workspaces[WS]!.supervisor!.state).toBe('serving')
  })

  it('a view with only the environment id still yields the link', () => {
    const e = reduce(emptyEntities(), { type: 'workspaces', at: 5, view: listBody(wsView({ environment_id: ENV, supervisor: null, session: null })) })
    expect(e.workspaces[WS]!.session).toMatchObject({ environmentId: ENV, url: `https://claude.ai/code?environment=${ENV}` })
  })

  it('a session.status without an id keeps the id it had', () => {
    const e = reduceAll(emptyEntities(), evs([...CLONE_OK, ...SERVE_OK]))
    const noId = { ...sessionEvent(31, 2, 4), data: { capacity_used: 2, capacity_total: 4, sessions: 1 } }
    expect(reduce(e, { type: 'event', event: noId }).workspaces[WS]!.session).toMatchObject({ environmentId: ENV, capacityUsed: 2 })
  })

  it('an unknown supervisor state is not guessed at', () => {
    const e = reduceAll(emptyEntities(), evs([...CLONE_OK, ...SERVE_OK, supEvent(30, 'failed', 'x')]))
    expect(e.workspaces[WS]!.supervisor!.state).toBe('serving')
  })
})
