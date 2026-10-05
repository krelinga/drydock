// The occupied count is counted on the client (lib/capacity.ts), so the rule
// it counts by must be the server's. Two checks: OCCUPYING against
// `workspace.Occupying`'s source, read here as text — a state the server
// starts counting fails this spec until the client counts it too — and the
// count itself against the server's own count of the same snapshot, which
// the mock backend computes as internal/workspace CapacityOf does.

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { WORKSPACE_STATES, type WorkspaceState } from '../api/types'
import { capacityView, newBackend, workspaceList } from '../mocks/backend'
import { emptyEntities, reduce, reduceAll } from '../stores/reducer'
import { CLONE_OK, STOP_OK, WS, listWithCap, stateEvent, wsView } from '../stores/reducer.fixtures'
import { OCCUPYING, capacity, needsSlot } from './capacity'

// Vitest runs from web/ (jsdom's import.meta.url is not a file URL).
const GO = readFileSync(resolve(process.cwd(), '../internal/workspace/state.go'), 'utf8')

describe('OCCUPYING is workspace.Occupying', () => {
  it('names the same states', () => {
    const fn = GO.slice(GO.indexOf('func Occupying(s State) bool {'))
    expect(fn.length, 'state.go has Occupying').toBeGreaterThan(0)
    const body = fn.slice(0, fn.indexOf('\n}\n'))
    const cases = [...body.matchAll(/^\s*case ([^:]+):\s*\n\s*return true/gm)].flatMap((m) => m[1]!.split(',').map((x) => x.trim()))
    // Control: the parse found the case, so an empty one cannot pass.
    expect(cases.length).toBeGreaterThan(0)
    // Go's constant names → their values, from the same file.
    const value = (name: string) => {
      const m = GO.match(new RegExp(`\\b${name}\\s+State = "([a-z]+)"`))
      expect(m, `state.go defines ${name}`).not.toBeNull()
      return m![1]! as WorkspaceState
    }
    expect([...OCCUPYING].sort()).toEqual(cases.map(value).sort())
    // And nothing the client counts is a state the server does not have.
    for (const s of OCCUPYING) expect(WORKSPACE_STATES).toContain(s)
  })
})

describe('the count', () => {
  it('counts the workspaces in an occupying state, and is full at the cap', () => {
    const e = reduce(emptyEntities(), {
      type: 'workspaces', at: 50,
      view: listWithCap(2,
        wsView({ id: '01JA0000000000000000000021', state: 'running' }),
        wsView({ id: '01JA0000000000000000000022', state: 'building' }),
        wsView({ id: '01JA0000000000000000000023', state: 'stopped' }),
        wsView({ id: '01JA0000000000000000000024', state: 'failed' }),
        wsView({ id: '01JA0000000000000000000025', state: 'deleting' })),
    })
    expect(capacity(e)).toEqual({ cap: 2, occupied: 2, full: true })
    // Control: one more slot and it is not full.
    expect(capacity({ ...e, cap: 3 })).toEqual({ cap: 3, occupied: 2, full: false })
    // Unknown cap is never full, whatever the count.
    expect(capacity({ ...e, cap: null }).full).toBe(false)
  })

  it('stays live from workspace.state events alone', () => {
    const start = reduce(emptyEntities(), { type: 'workspaces', at: 0, view: listWithCap(1, wsView({ state: 'stopped' })) })
    expect(capacity(start).occupied).toBe(0)
    const events = (evs: ReturnType<typeof stateEvent>[]) => evs.map((event) => ({ type: 'event' as const, event }))
    const building = reduceAll(start, events([stateEvent(60, WS, 'building', { from: 'stopped' })]))
    expect(capacity(building)).toEqual({ cap: 1, occupied: 1, full: true })
    const stopped = reduceAll(building, events([stateEvent(61, WS, 'running', { from: 'building' }), stateEvent(62, WS, 'stopped', { from: 'running' })]))
    expect(capacity(stopped)).toEqual({ cap: 1, occupied: 0, full: false })
  })

  it("agrees with the server's own count of every snapshot the mock serves", () => {
    const b = newBackend()
    b.signedIn = true
    // Every state at least once.
    WORKSPACE_STATES.forEach((state, i) => {
      const id = `01JD${String(i).padStart(22, '0')}`
      b.workspaces[id] = { id, repository_id: 1, branch: 'main', state, state_detail: null, container_id: null, created_at: '2026-10-04T12:00:00Z', steps: {} }
    })
    const body = workspaceList(b)
    const e = reduce(emptyEntities(), { type: 'workspaces', at: 0, view: body })
    expect(capacity(e).occupied).toBe(capacityView(b).occupied)
    expect(capacity(e).occupied).toBe(body.capacity!.occupied)
    // Control: the snapshot had something in every state, so the two
    // agreeing is not two zeros.
    expect(body.capacity!.occupied).toBeGreaterThan(0)
    expect(body.capacity!.occupied).toBeLessThan(body.workspaces.length)
  })
})

describe('needsSlot', () => {
  it('a clone and a start take a slot, a rebuild only of a workspace not running; stop and delete never', () => {
    const running = reduceAll(emptyEntities(), CLONE_OK.map((event) => ({ type: 'event' as const, event }))).workspaces[WS]!
    const stopped = reduceAll(emptyEntities(), [...CLONE_OK, ...STOP_OK].map((event) => ({ type: 'event' as const, event }))).workspaces[WS]!
    expect(needsSlot('clone', null)).toBe(true)
    expect(needsSlot('start', stopped)).toBe(true)
    expect(needsSlot('rebuild', stopped)).toBe(true)
    expect(needsSlot('rebuild', running)).toBe(false)
    expect(needsSlot('stop', running)).toBe(false)
    expect(needsSlot('delete', running)).toBe(false)
    expect(needsSlot(null, running)).toBe(false)
  })
})
