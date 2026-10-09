// The reducer's port cases (frontend §4.1, §6.3; port forwarding §13 step 4).
// Each frame's `data` is what internal/preview's registry writes: the whole
// row as `data.port`, or `{port_id, container_port}` for port.retired. Every
// negative case keeps a positive control in the same test.

import { describe, expect, it } from 'vitest'
import type { PortList, PortView, StreamEvent } from '../api/types'
import { emptyEntities, portsOf, reduce, reduceAll, type Action, type Entities } from './reducer'
import { OVER_PORT, portEntityOutcome, portEventOutcome, settlesAdd, settlesPort } from './ports'

const WS = '01JA0000000000000000000001'
const OTHER = '01JA0000000000000000000002'
const at = (n: number) => new Date(Date.UTC(2026, 9, 9, 9, 0, n)).toISOString()

function port(id: string, n: number, over: Partial<PortView> = {}): PortView {
  return {
    id, workspace_id: WS, container_port: n, slug: `myapp-${n}-abcd`, host: `myapp-${n}-abcd.drydock-preview.test`,
    url: null, label: null, upstream_scheme: 'http', host_header: 'localhost', enabled: false, hidden: false,
    declared: true, observed: false, manual: false, bind_addr: null, observed_state: null, last_seen_at: null,
    created_at: at(0), ...over,
  }
}
const on = (p: PortView): PortView => ({ ...p, enabled: true, url: `https://${p.host}/` })

function ev(id: number, kind: string, data: Record<string, unknown>, ws = WS): StreamEvent {
  return { id, kind, message: kind, level: 'info', at: at(id), workspace_id: ws, data }
}
const events = (evs: StreamEvent[]): Action[] => evs.map((event) => ({ type: 'event', event }))
const play = (evs: StreamEvent[], start: Entities = emptyEntities()) => reduceAll(start, events(evs))
const list = (atN: number, ports: PortView[], previews = true): Action =>
  ({ type: 'ports', at: atN, workspaceId: WS, view: { ports, previews } satisfies PortList })

describe('port events', () => {
  it('added, enabled, disabled, retired, in order', () => {
    const p = port('P1', 5173)
    let e = play([ev(1, 'port.added', { port: p }), ev(2, 'port.enabled', { port: on(p) })])
    expect(e.ports.P1).toMatchObject({ containerPort: 5173, enabled: true, url: 'https://myapp-5173-abcd.drydock-preview.test/', at: 2 })
    e = reduce(e, { type: 'event', event: ev(3, 'port.disabled', { port: p }) })
    expect(e.ports.P1).toMatchObject({ enabled: false, url: null, at: 3 })
    e = reduce(e, { type: 'event', event: ev(4, 'port.retired', { port_id: 'P1', container_port: 5173 }) })
    expect(e.ports.P1).toBeUndefined()
    expect(e.portsRetired.P1).toBe(4)
    // They joined the workspace's feed, as every event naming it does.
    expect(e.feeds[WS]!.map((x) => x.id)).toEqual([4, 3, 2, 1])
  })

  it('an older event cannot roll a port back, and a replay changes nothing', () => {
    const p = port('P1', 5173)
    const enabled = ev(5, 'port.enabled', { port: on(p) })
    const added = ev(4, 'port.added', { port: p })
    expect(play([enabled, added]).ports.P1!.enabled).toBe(true)
    // Control: in order, the newer still wins, and replaying both is a no-op.
    const e = play([added, enabled])
    expect(e.ports.P1!.enabled).toBe(true)
    expect(play([added, enabled], e)).toBe(e)
  })

  it('a retired port never comes back — not by a late event, not by an older list', () => {
    const p = port('P1', 5173)
    let e = play([ev(1, 'port.added', { port: p }), ev(3, 'port.retired', { port_id: 'P1', container_port: 5173 })])
    e = reduce(e, { type: 'event', event: ev(2, 'port.enabled', { port: on(p) }) })
    expect(e.ports.P1).toBeUndefined()
    e = reduce(e, list(2, [p]))
    expect(e.ports.P1).toBeUndefined()
    // Control: the port listed again is a new row, and it is shown.
    e = reduce(e, { type: 'event', event: ev(6, 'port.added', { port: port('P2', 5173) }) })
    expect(portsOf(e, WS).map((x) => x.id)).toEqual(['P2'])
  })

  it('a row whose url is not https keeps no link; one for another workspace is ignored', () => {
    const e = play([
      ev(1, 'port.enabled', { port: { ...on(port('P1', 1)), url: 'javascript:alert(1)' } }),
      ev(2, 'port.enabled', { port: on(port('P2', 2)) }, OTHER),
    ])
    expect(e.ports.P1!.url).toBeNull()
    expect(e.ports.P1!.enabled).toBe(true)
    expect(e.ports.P2).toBeUndefined()
    // Control: an https URL is kept.
    expect(play([ev(1, 'port.enabled', { port: on(port('P1', 1)) })]).ports.P1!.url).toMatch(/^https:\/\//)
  })

  it('workspace.gone takes its ports, and no late port event brings one back', () => {
    let e = play([ev(1, 'port.added', { port: port('P1', 1) }), ev(2, 'workspace.gone', {})])
    expect(e.ports).toEqual({})
    e = reduce(e, { type: 'event', event: ev(3, 'port.enabled', { port: on(port('P1', 1)) }) })
    expect(e.ports).toEqual({})
    // Control: another workspace's port is untouched by this one's gone.
    const other = play([ev(1, 'port.added', { port: { ...port('Q1', 1), workspace_id: OTHER } }, OTHER), ev(2, 'workspace.gone', {})])
    expect(other.ports.Q1).toBeDefined()
  })
})

describe('the port list', () => {
  it('is the authority on which of the workspace\'s ports exist, unless an event is newer', () => {
    let e = play([ev(1, 'port.added', { port: port('P1', 1) }), ev(5, 'port.added', { port: port('P5', 5) })])
    e = reduce(e, { type: 'event', event: ev(2, 'port.added', { port: { ...port('Q1', 9), workspace_id: OTHER } }, OTHER) })
    e = reduce(e, list(3, [port('P3', 3)]))
    // P1 is not listed and nothing newer wrote it: gone. P5 is newer than the list: kept.
    expect(portsOf(e, WS).map((p) => p.id)).toEqual(['P3', 'P5'])
    // Another workspace's ports are not this list's to drop.
    expect(e.ports.Q1).toBeDefined()
    expect(e.portsLoaded[WS]).toBe(3)
    expect(e.previews).toBe(true)
  })

  it('does not roll back a port an event newer than it wrote', () => {
    let e = play([ev(4, 'port.enabled', { port: on(port('P1', 1)) })])
    e = reduce(e, list(2, [port('P1', 1)]))
    expect(e.ports.P1!.enabled).toBe(true)
    // Control: a list newer than the event is applied.
    e = reduce(e, list(7, [port('P1', 1, { hidden: true })]))
    expect(e.ports.P1).toMatchObject({ enabled: false, hidden: true, at: 7 })
  })

  it('carries whether previews are configured', () => {
    expect(reduce(emptyEntities(), list(1, [], false)).previews).toBe(false)
    expect(emptyEntities().previews).toBeNull()
  })
})

describe('what ends a port mark', () => {
  const p = port('P1', 5173)
  it('enable ends on port.enabled for that row, or its retirement — never another row\'s', () => {
    const settles = settlesPort(WS, 'P1', OVER_PORT.enabled(true))
    expect(settles(ev(2, 'port.enabled', { port: on(port('P2', 80)) }))).toBe(false)
    expect(settles(ev(2, 'port.disabled', { port: p }))).toBe(false)
    expect(settles(ev(2, 'port.enabled', { port: on(p) }))).toBe(true)
    expect(settles(ev(3, 'port.retired', { port_id: 'P1' }))).toBe(true)
    expect(settles(ev(3, 'workspace.gone', {}))).toBe(true)
    expect(settles(ev(3, 'workspace.gone', {}, OTHER))).toBe(false)
  })
  it('the snapshot path asks the same predicate of the entities', () => {
    const e = play([ev(1, 'port.added', { port: p })])
    expect(OVER_PORT.enabled(true)(portEntityOutcome(e, WS, 'P1'))).toBe(false)
    expect(OVER_PORT.enabled(false)(portEntityOutcome(e, WS, 'P1'))).toBe(true)
    expect(OVER_PORT.retired(portEntityOutcome(e, WS, 'nosuch'))).toBe(true)
    expect(portEventOutcome(WS, 'P1', ev(2, 'workspace.state', { state: 'stopped' }))).toBeNull()
  })
  it('an add ends on port.added for that number on that workspace', () => {
    const settles = settlesAdd(WS, 8080)
    expect(settles(ev(1, 'port.added', { port: port('P1', 5173) }))).toBe(false)
    expect(settles(ev(1, 'port.added', { port: { ...port('Q', 8080), workspace_id: OTHER } }, OTHER))).toBe(false)
    expect(settles(ev(1, 'port.added', { port: port('P8', 8080) }))).toBe(true)
  })
})