// §6.1's state table, the workspace half, as a parameterized test: every
// state renders one status line and at most one action.

import { describe, expect, it } from 'vitest'
import { emptyEntities, reduce, reduceAll, type Repo, type Workspace } from '../stores/reducer'
import type { StreamEvent } from '../api/types'
import {
  CLONE_FAILS_AT_UP, CLONE_OK, DELETE_RESUMED, DELETE_STUCK, STOP_FAILED_DETAIL, STOP_FAILS, STOP_OK, STUCK_DETAIL, WS,
  WS2, listBody, stateEvent, ws2View, wsView,
} from '../stores/reducer.fixtures'
import { cardStatus, rowAction, stoppable, withRoom } from './workspaceCard'

const at = (n: number) => reduceAll(emptyEntities(), CLONE_OK.slice(0, n).map((event) => ({ type: 'event' as const, event }))).workspaces[WS]!

function withState(state: string): Workspace {
  return reduceAll(emptyEntities(), [...CLONE_OK, stateEvent(99, WS, state)].map((event) => ({ type: 'event' as const, event }))).workspaces[WS]!
}

describe('the card, workspace half (§6.1)', () => {
  it.each([
    ['running', 'Running', 'ok', 'stop'],
    ['stopped', 'Stopped', 'idle', 'start'],
    ['deleting', 'Deleting…', 'idle', null],
    ['pending', 'Waiting to start', 'busy', null],
  ])('%s → "%s", %s, action %s', (state, line, tone, action) => {
    const s = cardStatus(withState(state))
    expect(s).toMatchObject({ line, tone, action })
  })

  it('a moving workspace names the step it is in, and only while it is running', () => {
    expect(cardStatus(at(5)).line).toBe('Cloning · cloning…') // clone started
    expect(cardStatus(at(10)).line).toBe('Building · starting the container…') // up started
    // Control: with the step done and nothing started, just the state.
    expect(cardStatus(at(6)).line).toBe('Cloning')
  })

  it('failed names the step — from events, and from a snapshot alone', () => {
    const fromEvents = reduceAll(emptyEntities(), CLONE_FAILS_AT_UP.map((event) => ({ type: 'event' as const, event }))).workspaces[WS2]!
    expect(cardStatus(fromEvents)).toMatchObject({
      line: 'Failed while starting the container', tone: 'bad', action: 'rebuild',
      note: 'The container start step failed.',
    })
    const fromList = reduce(emptyEntities(), { type: 'workspaces', at: 30, view: listBody(ws2View()) }).workspaces[WS2]!
    expect(cardStatus(fromList).line).toBe('Failed while starting the container')
    // Control: with nothing naming a step, it says Failed and invents none.
    const bare = reduce(emptyEntities(), { type: 'workspaces', at: 30, view: listBody(ws2View({ steps: {} })) }).workspaces[WS2]!
    expect(cardStatus(bare).line).toBe('Failed')
  })

  it('stopped says what survived', () => {
    expect(cardStatus(withState('stopped')).note).toBe('The clone is intact.')
  })

  it('running claims nothing about a session: the supervisor half is a seam', () => {
    const s = cardStatus(withState('running'))
    expect(s.line).not.toMatch(/session|capacity|claude/i)
    // Its action is the workspace's own, Stop — never a session action.
    expect(s.action).toBe('stop')
    // Control: the line is the workspace state's own.
    expect(s.line).toBe('Running')
  })
})

describe('the card while a stop or a delete runs (Phase 6)', () => {
  const ev = (evs: StreamEvent[]) => evs.map((event) => ({ type: 'event' as const, event }))
  const wsAfter = (evs: StreamEvent[], id = WS) => reduceAll(emptyEntities(), ev([...CLONE_OK, ...evs])).workspaces[id]!
  const listed = (w: Workspace) => ({ ...w, fullName: 'krelinga/drydock' })

  it('a stopping workspace names its sub-step and offers nothing; stopped offers Start', () => {
    expect(cardStatus(wsAfter(STOP_OK.slice(0, 3)))).toMatchObject({
      line: 'Stopping · stopping the container…', tone: 'busy', action: null,
    })
    // Control: once the move lands, it is Stopped with Start.
    expect(cardStatus(wsAfter(STOP_OK))).toMatchObject({ line: 'Stopped', action: 'start' })
  })

  it('a stop that failed says where, keeps the workspace running, and offers Stop again', () => {
    const s = cardStatus(wsAfter(STOP_FAILS))
    expect(s).toMatchObject({
      line: 'Stop failed while stopping the container', tone: 'bad', action: 'stop', note: STOP_FAILED_DETAIL,
    })
    expect(wsAfter(STOP_FAILS).state).toBe('running')
    // Between the failed sub-step and its annotation the run says the same line.
    expect(cardStatus(wsAfter(STOP_FAILS.slice(0, 4)))).toMatchObject({
      line: 'Stop failed while stopping the container', tone: 'bad', action: 'stop',
      note: "docker could not stop the workspace's container.",
    })
  })

  it('a stop that failed reads the same from the list alone, after a reload (§4.5 #15)', () => {
    const last_action = { action: 'stop', step: 'container', status: 'failed' as const, at: '2026-10-04T12:01:23Z' }
    const cold = reduce(emptyEntities(), { type: 'workspaces', at: 84, view: listBody(wsView({ state_detail: STOP_FAILED_DETAIL, last_action })) }).workspaces[WS]!
    expect(cardStatus(cold)).toEqual(cardStatus(wsAfter(STOP_FAILS)))
    // Control: running with a detail that is not a failed stop (an adoption) is Running.
    const adopted = reduce(emptyEntities(), { type: 'workspaces', at: 5, view: listBody(wsView({ state_detail: 'Found with no record; adopted from its labels.', last_action: null })) }).workspaces[WS]!
    expect(cardStatus(adopted)).toMatchObject({ line: 'Running', tone: 'ok', action: 'stop', note: 'Found with no record; adopted from its labels.' })
  })

  it('a running delete names its sub-step; a stuck one says so and offers Delete again', () => {
    expect(cardStatus(listed(wsAfter(DELETE_STUCK.slice(0, 4))))).toMatchObject({
      line: 'Deleting · removing the containers…', tone: 'busy', action: null,
    })
    // The failed sub-step alone, before the annotation: still the delete's.
    expect(cardStatus(listed(wsAfter(DELETE_STUCK.slice(0, 9)))).action).toBeNull()
    const stuck = cardStatus(listed(wsAfter(DELETE_STUCK)))
    expect(stuck).toMatchObject({ line: 'Delete stopped part-way', tone: 'bad', action: 'delete', note: STUCK_DETAIL })
    // A resume running — here or elsewhere — is Deleting again, nothing to press.
    expect(cardStatus(listed(wsAfter([...DELETE_STUCK, ...DELETE_RESUMED.slice(0, 4)])))).toMatchObject({
      line: 'Deleting · removing the containers…', action: null,
    })
    // Control: a plain deleting with no annotation is just Deleting….
    expect(cardStatus(listed(withState('deleting')))).toMatchObject({ line: 'Deleting…', action: null })
  })

  it('the row of a stuck delete offers Delete again too, and never Clone', () => {
    const w = listed(wsAfter(DELETE_STUCK))
    const repo: Repo = {
      id: 1, installationId: 101, fullName: 'krelinga/drydock', defaultBranch: 'main', private: true,
      archived: false, hasDevcontainer: true, pushedAt: null, removed: false,
    }
    expect(rowAction({ repo, workspace: w, held: true })).toBe('delete')
    // Control: with nothing holding it, the same repo clones.
    expect(rowAction({ repo, workspace: null, held: false })).toBe('clone')
  })
})

describe('the catalog row action: one repository, one workspace (design §5)', () => {
  const repo = (over: Partial<Repo> = {}): Repo => ({
    id: 1, installationId: 101, fullName: 'krelinga/drydock', defaultBranch: 'main', private: true,
    archived: false, hasDevcontainer: true, pushedAt: null, removed: false, ...over,
  })

  it.each([
    ['pending', null], ['cloning', null], ['building', null], ['running', 'stop'],
    ['stopped', 'start'], ['failed', 'rebuild'], ['deleting', null],
  ])('a %s workspace holds the repository: %s, never clone', (state, action) => {
    expect(rowAction({ repo: repo(), workspace: withState(state), held: true })).toBe(action)
  })

  it('offers Clone only where nothing holds the repository, and never on a removed one', () => {
    // Control: no workspace at all is the one case that clones.
    expect(rowAction({ repo: repo(), workspace: null, held: false })).toBe('clone')
    // A workspace whose state is not yet known (a stub) still holds it.
    expect(rowAction({ repo: repo(), workspace: null, held: true })).toBeNull()
    expect(rowAction({ repo: repo({ removed: true }), workspace: null, held: false })).toBeNull()
  })
})

describe('at the cap (design §1, frontend §9)', () => {
  it('an action that would take a slot becomes make_room; one that would not is kept', () => {
    const stopped = withState('stopped')
    const running = withState('running')
    const failed = reduceAll(emptyEntities(), CLONE_FAILS_AT_UP.map((event) => ({ type: 'event' as const, event }))).workspaces[WS2]!
    expect(withRoom('clone', null, true)).toBe('make_room')
    expect(withRoom('start', stopped, true)).toBe('make_room')
    expect(withRoom('rebuild', failed, true)).toBe('make_room')
    expect(withRoom('rebuild', stopped, true)).toBe('make_room')
    // A running workspace's rebuild keeps its own slot; stop and delete free one.
    expect(withRoom('rebuild', running, true)).toBe('rebuild')
    expect(withRoom('stop', running, true)).toBe('stop')
    expect(withRoom('delete', running, true)).toBe('delete')
    expect(withRoom(null, running, true)).toBeNull()
    // Control: under the cap, everything is itself.
    for (const [a, w] of [['clone', null], ['start', stopped], ['rebuild', failed]] as const) {
      expect(withRoom(a, w, false)).toBe(a)
    }
  })

  it('stoppable is exactly the cards whose action is Stop', () => {
    expect(stoppable(withState('running'))).toBe(true)
    const ev = (evs: StreamEvent[]) => evs.map((event) => ({ type: 'event' as const, event }))
    // A failed stop still offers Stop, so it is still one to stop.
    expect(stoppable(reduceAll(emptyEntities(), ev([...CLONE_OK, ...STOP_FAILS])).workspaces[WS]!)).toBe(true)
    // Control: stopping, building, failed and deleting are not.
    expect(stoppable(reduceAll(emptyEntities(), ev([...CLONE_OK, ...STOP_OK.slice(0, 3)])).workspaces[WS]!)).toBe(false)
    for (const state of ['building', 'failed', 'deleting', 'stopped']) expect(stoppable(withState(state))).toBe(false)
  })
})
