// Resources through the reducer (design §6 *Resources*, frontend §6.1): the
// stream's named `resources` frames and the views' copies, versioned by the
// server's (boot, round), never by event id or wall clock, and never written
// for a workspace the entities do not hold.

import { describe, expect, it } from 'vitest'
import type { HostDiskView, ResourcesFrame, ResourcesView } from '../api/types'
import { CLONE_OK, WS, WS2, detailBody, listBody, stateEvent, wsView } from './reducer.fixtures'
import { emptyEntities, reduce, reduceAll, type Action, type Entities } from './reducer'

const BOOT = 'b00t'
const T = (s: number) => new Date(Date.UTC(2026, 9, 8, 12, 0, s)).toISOString()

function res(round: number, mem: number | null, disk: number | null, boot = BOOT): ResourcesView {
  return {
    boot, round,
    memory: mem === null ? null : { bytes: mem, at: T(round), stale: false },
    disk: disk === null ? null : {
      bytes: disk, directory_bytes: disk, container_bytes: null, partial: false, at: T(round), stale: false,
    },
  }
}

const host = (round: number, over = false, boot = BOOT): HostDiskView =>
  ({ used_bytes: over ? 95 : 50, total_bytes: 100, limit_percent: 90, over, at: T(round), boot, round })

function frame(round: number, workspaces: Record<string, ResourcesView>, over = false, boot = BOOT): ResourcesFrame {
  return { boot, round, at: T(round), workspaces, host: host(round, over, boot) }
}

const running = (): Entities => reduceAll(emptyEntities(), CLONE_OK.map((event): Action => ({ type: 'event', event })))

describe('resources', () => {
  it('a frame writes each known workspace and the host, and moves no stream position', () => {
    const before = running()
    const e = reduce(before, { type: 'resources', frame: frame(10, { [WS]: res(10, 1_200_000_000, 3_400_000_000) }, true) })
    expect(e.resources[WS]?.memory?.bytes).toBe(1_200_000_000)
    expect(e.resources[WS]?.disk?.bytes).toBe(3_400_000_000)
    expect(e.hostDisk?.over).toBe(true)
    expect(e.lastEventId).toBe(before.lastEventId)
    expect(e.workspaces[WS]).toBe(before.workspaces[WS]) // a measurement changes nothing about the workspace
  })

  it('an older round never replaces a newer one, from a frame or a snapshot', () => {
    let e = reduce(running(), { type: 'resources', frame: frame(20, { [WS]: res(20, 2, 2) }) })
    e = reduce(e, { type: 'resources', frame: frame(10, { [WS]: res(10, 1, 1) }) })
    expect(e.resources[WS]?.memory?.bytes).toBe(2)
    e = reduce(e, { type: 'workspaces', at: e.lastEventId, view: { ...listBody(wsView({ resources: res(15, 9, 9) })), disk: host(15, true) } })
    expect(e.resources[WS]?.memory?.bytes).toBe(2)
    expect(e.hostDisk?.round).toBe(20)
    expect(e.hostDisk?.over).toBe(false)
    // Control: a later round from the detail does replace it.
    e = reduce(e, { type: 'workspace', at: e.lastEventId, view: detailBody({ resources: res(30, 3, 3) }) })
    expect(e.resources[WS]?.memory?.bytes).toBe(3)
  })

  it('a new boot replaces whatever round the last one reached: a restart starts counting again', () => {
    let e = reduce(running(), { type: 'resources', frame: frame(500, { [WS]: res(500, 2, 2) }) })
    e = reduce(e, { type: 'resources', frame: frame(1, { [WS]: res(1, 7, 7, 'n3w'), }, false, 'n3w') })
    expect(e.resources[WS]?.memory?.bytes).toBe(7)
    expect(e.hostDisk?.boot).toBe('n3w')
  })

  it('the list carries the host disk and each workspace’s copy', () => {
    const body = { ...listBody(wsView({ resources: res(5, 7, 8) })), disk: host(5, true) }
    const e = reduce(emptyEntities(), { type: 'workspaces', at: 0, view: body })
    expect(e.resources[WS]?.disk?.bytes).toBe(8)
    expect(e.hostDisk?.over).toBe(true)
  })

  it('no reading stays no reading: null is never turned into zero', () => {
    const e = reduce(running(), { type: 'resources', frame: frame(10, { [WS]: res(10, null, null) }) })
    expect(e.resources[WS]?.memory).toBeNull()
    expect(e.resources[WS]?.disk).toBeNull()
    const bad = reduce(running(), { type: 'resources', frame: frame(10, { [WS]: { ...res(10, 1, 1), memory: { bytes: 'x' } } as never }) })
    expect(bad.resources[WS]?.memory).toBeNull()
    expect(bad.resources[WS]?.disk?.bytes).toBe(1) // control: the well-formed half is kept
  })

  it('a workspace the entities do not hold gets nothing, and a deleted one is never brought back', () => {
    let e = reduce(running(), { type: 'resources', frame: frame(10, { [WS2]: res(10, 1, 1), [WS]: res(10, 2, 2) }) })
    expect(e.resources[WS2]).toBeUndefined()
    expect(e.workspaces[WS2]).toBeUndefined()
    expect(e.resources[WS]).toBeDefined() // control
    e = reduce(e, { type: 'event', event: { ...stateEvent(20, WS, 'deleting'), kind: 'workspace.gone', data: {} } })
    expect(e.resources[WS]).toBeUndefined()
    e = reduce(e, { type: 'resources', frame: frame(30, { [WS]: res(30, 2, 2) }) })
    expect(e.resources[WS]).toBeUndefined()
    expect(e.workspaces[WS]).toBeUndefined()
  })

  it('a frame that is not one changes nothing', () => {
    const before = running()
    for (const f of [null, 'x', 7, { workspaces: 3 }, { workspaces: { [WS]: { round: 'never' } } }, { workspaces: { [WS]: { round: 3 } } }]) {
      expect(reduce(before, { type: 'resources', frame: f })).toBe(before)
    }
  })
})
