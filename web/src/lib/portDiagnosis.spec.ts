// The row's diagnosis (port forwarding §11, §13 step 6) as a pure function of
// structured fields, and the loopback sentence held to the Go that prints it
// on the preview's own page: change internal/preview's LoopbackSentence and
// this fails until the panel says the same.

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import type { Port } from '../stores/reducer'
import { bindWhere, diagnose, loopbackSentence } from './portDiagnosis'

function p(over: Partial<Port>): Port {
  return {
    id: 'P', workspaceId: 'W', containerPort: 5173, slug: 's', host: null, url: null, label: null, hostHeader: 'localhost',
    enabled: false, hidden: false, declared: false, observed: true, manual: false, bindAddr: null, loopback: false,
    observedState: null, lastSeenAt: null, at: 1, ...over,
  }
}

describe('diagnose', () => {
  it('a loopback-only listener, enabled or not, running or not', () => {
    const loop = p({ observedState: 'listening', bindAddr: '127.0.0.1', loopback: true })
    expect(diagnose(loop, true, 'ok')).toEqual({ kind: 'loopback', where: '127.0.0.1:5173' })
    expect(diagnose({ ...loop, enabled: true }, false, 'unavailable')).toEqual({ kind: 'loopback', where: '127.0.0.1:5173' })
    // Control: on 0.0.0.0, nothing to say; and a loopback bind seen gone is history.
    expect(diagnose(p({ observedState: 'listening', bindAddr: '0.0.0.0' }), true, 'ok')).toBeNull()
    expect(diagnose(p({ observedState: 'gone', bindAddr: '127.0.0.1', loopback: true }), true, 'ok')).toBeNull()
  })

  it('an enabled port gone, or never seen while discovery works', () => {
    const gone = p({ enabled: true, observedState: 'gone', bindAddr: '0.0.0.0', lastSeenAt: '2026-10-09T12:00:00Z' })
    expect(diagnose(gone, true, 'ok')).toEqual({ kind: 'not_listening', lastSeenAt: '2026-10-09T12:00:00Z' })
    expect(diagnose(p({ enabled: true }), true, 'ok')).toEqual({ kind: 'never_listened' })
    // Controls: off, stopped (the panel's one sentence), and a scanner that cannot read or has not said.
    expect(diagnose({ ...gone, enabled: false }, true, 'ok')).toBeNull()
    expect(diagnose(gone, false, 'ok')).toBeNull()
    expect(diagnose(p({ enabled: true }), true, 'unavailable')).toBeNull()
    expect(diagnose(p({ enabled: true }), true, null)).toBeNull()
  })

  it('names the address as the dev server prints it', () => {
    expect(bindWhere('127.0.0.1', 5173)).toBe('127.0.0.1:5173')
    expect(bindWhere('::1', 5173)).toBe('[::1]:5173')
    expect(bindWhere('::ffff:127.0.0.1', 5173)).toBe('127.0.0.1:5173')
    expect(bindWhere(null, 5173)).toBe('5173')
  })

  it("says what the proxy's page says", () => {
    const go = readFileSync(resolve(process.cwd(), '../internal/preview/proxy.go'), 'utf8')
    const m = /func LoopbackSentence[\s\S]*?fmt\.Sprintf\("([^"]+)", where\)/.exec(go)
    expect(m).not.toBeNull()
    expect(loopbackSentence('127.0.0.1:5173')).toBe(m![1]!.replace('%s', '127.0.0.1:5173'))
  })
})
