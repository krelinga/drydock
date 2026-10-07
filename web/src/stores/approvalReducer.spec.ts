// The host-access request on a workspace entity (design §6): written by the
// workspace.state event that stopped the run and by the views' `approval`,
// taken away by every other state event, and versioned with the state — so a
// replay is a no-op and an older input cannot bring a request back.

import { describe, expect, it } from 'vitest'
import type { ApprovalView, StreamEvent } from '../api/types'
import { cardStatus } from '../lib/workspaceCard'
import { emptyEntities, reduce, reduceAll, type Action } from './reducer'
import { CLONE_OK, WS, listBody, wsView } from './reducer.fixtures'
import { eventOutcome, OVER } from './workspaces'

const evs = (list: StreamEvent[]): Action[] => list.map((event) => ({ type: 'event' as const, event }))
const REQ: ApprovalView = {
  hash: 'sha256:' + 'a'.repeat(64),
  added: [{ field: 'privileged', source: 'feature_or_image', value: true }],
  changed: [], removed: [],
}
const state = (id: number, data: Record<string, unknown>): StreamEvent =>
  ({ id, kind: 'workspace.state', workspace_id: WS, level: 'info', message: 'm', at: '2026-10-04T12:01:00Z', data })

describe('the approval request', () => {
  it('is set by the stopping event and cleared by the next move; a replay changes nothing', () => {
    const asked = reduceAll(emptyEntities(), evs([...CLONE_OK,
      state(100, { state: 'stopped', from: 'building', detail: 'asks', approval: REQ })]))
    const w = asked.workspaces[WS]!
    expect(w.approval).toEqual(REQ)
    expect(cardStatus(w)).toMatchObject({ line: 'Needs approval', action: 'approve' })
    expect(reduceAll(asked, evs([state(100, { state: 'stopped', from: 'building', detail: 'asks', approval: REQ })])).workspaces[WS]).toBe(w)
    const moved = reduce(asked, { type: 'event', event: state(101, { state: 'building', from: 'stopped' }) })
    expect(moved.workspaces[WS]!.approval).toBeNull()
    // An older event cannot bring it back.
    const late = reduce(moved, { type: 'event', event: state(100, { state: 'stopped', from: 'building', approval: REQ }) })
    expect(late.workspaces[WS]!.approval).toBeNull()
  })

  it('a stopped workspace without one offers Start (control), and a malformed one is no request', () => {
    const e = reduceAll(emptyEntities(), evs([...CLONE_OK, state(100, { state: 'stopped', from: 'running' })]))
    expect(cardStatus(e.workspaces[WS]!)).toMatchObject({ line: 'Stopped', action: 'start' })
    const bad = reduce(e, { type: 'event', event: state(101, { state: 'stopped', from: 'stopped', approval: { added: [] } }) })
    expect(bad.workspaces[WS]!.approval).toBeNull()
  })

  it('comes from a view, and a newer event beats an older snapshot', () => {
    const e = reduce(emptyEntities(), { type: 'workspaces', at: 50, view: listBody(wsView({ state: 'stopped', approval: REQ })) })
    expect(e.workspaces[WS]!.approval).toEqual(REQ)
    const cleared = reduce(e, { type: 'event', event: state(60, { state: 'stopped', from: 'stopped', detail: 'declined' }) })
    const old = reduce(cleared, { type: 'workspaces', at: 55, view: listBody(wsView({ state: 'stopped', approval: REQ })) })
    expect(old.workspaces[WS]!.approval).toBeNull()
  })

  it('a decline is over when the request is gone, not before', () => {
    const o = eventOutcome(WS, state(1, { state: 'stopped', from: 'building', approval: REQ }))!
    expect(OVER.decline(o)).toBe(false)
    expect(OVER.decline(eventOutcome(WS, state(2, { state: 'stopped', from: 'stopped', detail: 'd' }))!)).toBe(true)
    // An approval continues a build: over when it leaves it, including to ask again.
    expect(OVER.build(eventOutcome(WS, state(3, { state: 'building', from: 'stopped' }))!)).toBe(false)
    expect(OVER.build(o)).toBe(true)
  })
})
