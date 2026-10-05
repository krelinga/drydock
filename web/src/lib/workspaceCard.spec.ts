// §6.1's state table, the workspace half, as a parameterized test: every
// state renders one status line and at most one action.

import { describe, expect, it } from 'vitest'
import { emptyEntities, reduce, reduceAll, type Repo, type Workspace } from '../stores/reducer'
import { CLONE_FAILS_AT_UP, CLONE_OK, WS, WS2, listBody, stateEvent, ws2View } from '../stores/reducer.fixtures'
import { cardStatus, rowAction } from './workspaceCard'

const at = (n: number) => reduceAll(emptyEntities(), CLONE_OK.slice(0, n).map((event) => ({ type: 'event' as const, event }))).workspaces[WS]!

function withState(state: string): Workspace {
  return reduceAll(emptyEntities(), [...CLONE_OK, stateEvent(99, WS, state)].map((event) => ({ type: 'event' as const, event }))).workspaces[WS]!
}

describe('the card, workspace half (§6.1)', () => {
  it.each([
    ['running', 'Running', 'ok', null],
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
      line: 'Failed while starting the container', tone: 'bad', action: 'start',
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
    expect(s.action).toBeNull()
    // Control: the line is the workspace state's own.
    expect(s.line).toBe('Running')
  })
})

describe('the catalog row action: one repository, one workspace (design §5)', () => {
  const repo = (over: Partial<Repo> = {}): Repo => ({
    id: 1, installationId: 101, fullName: 'krelinga/drydock', defaultBranch: 'main', private: true,
    archived: false, hasDevcontainer: true, pushedAt: null, removed: false, ...over,
  })

  it.each([
    ['pending', null], ['cloning', null], ['building', null], ['running', null],
    ['stopped', 'start'], ['failed', 'start'], ['deleting', null],
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
