// The mock backend's port discovery against the server's (port forwarding
// §13 step 5): internal/preview's TestDiscoveryEventsGolden records the
// events a real preview.Scanner and preview.Service write for one scenario in
// internal/preview/testdata/discovery-events.json, and this spec plays the
// same scenario on the mock and demands the same events — kinds, levels, the
// data's keys and the row's fields — so a change to either side fails until
// the other matches. A mock that announced more, or listed a port sooner,
// would pass specs a real server fails.

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import type { StreamEvent } from '../api/types'
import { addMockPort, newBackend, scanPorts, WS_RUNNING } from './backend'

const GOLDEN = JSON.parse(readFileSync(resolve(process.cwd(), '../internal/preview/testdata/discovery-events.json'), 'utf8')) as unknown[]

const PORT_FIELDS = ['container_port', 'enabled', 'declared', 'observed', 'manual', 'hidden', 'observed_state', 'bind_addr', 'loopback', 'label', 'url']

/** One event as the Go test's goldenEvent writes it, `scan` being which scan wrote it; omitempty fields left out. */
function normalize(e: StreamEvent, scan: number): Record<string, unknown> {
  const data = (e.data ?? {}) as Record<string, unknown>
  const out: Record<string, unknown> = { scan, kind: e.kind, level: e.level, data_keys: Object.keys(data).sort() }
  if (data.port !== undefined) {
    const p = data.port as Record<string, unknown>
    out.port = Object.fromEntries(PORT_FIELDS.map((k) => [k, p[k]]))
  }
  out.source = data.source ?? ''
  if (typeof data.discovery === 'string' && data.discovery !== '') out.discovery = data.discovery
  if (typeof data.container_port === 'number' && data.container_port !== 0) out.container_port = data.container_port
  return out
}

describe('mock discovery', () => {
  it('writes what the server writes for the golden scenario', () => {
    const b = newBackend()
    // internal/preview playGolden: 5173 declared, labelled; then a server on
    // it at 0.0.0.0 and another on 127.0.0.1:8080; a rescan; both stop.
    addMockPort(b, WS_RUNNING, 5173, { label: 'vite', declared: true })
    const got: Record<string, unknown>[] = []
    let scans = 0
    const scan = (asked: string[] = []) => {
      const from = b.events.length
      scanPorts(b, asked)
      scans++
      for (const e of b.events.slice(from)) got.push(normalize(e, scans))
    }
    b.sockets[WS_RUNNING] = [{ port: 5173, bind: '0.0.0.0' }, { port: 8080, bind: '127.0.0.1' }]
    // The server's scans, one per interval: playGolden's six, not the mock's
    // constants, so a mock that listed sooner or dropped later fails here.
    scan()
    scan()
    scan([WS_RUNNING])
    b.sockets[WS_RUNNING] = []
    scan()
    scan()
    scan()
    expect(GOLDEN.length).toBeGreaterThan(0)
    expect(got).toEqual(GOLDEN)
  })

  it('lists nothing after one scan, and nothing it never saw twice', () => {
    const b = newBackend()
    b.sockets[WS_RUNNING] = [{ port: 3000, bind: '0.0.0.0' }]
    scanPorts(b)
    expect(Object.values(b.ports).length).toBe(0)
    scanPorts(b) // the control
    expect(Object.values(b.ports).map((p) => [p.container_port, p.enabled, p.observed])).toEqual([[3000, false, true]])
  })

  it('never revives a retired row: the port seen again is a new row with a new slug', () => {
    const b = newBackend()
    b.sockets[WS_RUNNING] = [{ port: 3000, bind: '0.0.0.0' }]
    scanPorts(b)
    scanPorts(b)
    const old = Object.values(b.ports)[0]!
    b.ports[old.id] = { ...old, retired: true }
    scanPorts(b)
    const live = Object.values(b.ports).filter((p) => !p.retired)
    expect(live.length).toBe(1)
    expect(live[0]!.id).not.toBe(old.id)
    expect(live[0]!.slug).not.toBe(old.slug)
    expect(b.ports[old.id]!.retired).toBe(true)
  })
})
