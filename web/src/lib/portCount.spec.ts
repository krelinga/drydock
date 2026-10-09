// The card's ambient port count (port forwarding §8.2): counted from the
// port entities, listening and previewed, hidden rows not counted as
// listening, and nothing to say when there is nothing.

import { describe, expect, it } from 'vitest'
import type { Port } from '../stores/reducer'
import { portCount, portCountLabel, portCountText } from './portCount'

function p(over: Partial<Port>): Port {
  return {
    id: 'P', workspaceId: 'W', containerPort: 1, slug: 's', host: null, url: null, label: null, hostHeader: 'localhost',
    enabled: false, hidden: false, declared: false, observed: false, manual: false, bindAddr: null, loopback: false,
    observedState: null, lastSeenAt: null, at: 1, ...over,
  }
}

describe('portCount', () => {
  it('counts listening and previewed rows', () => {
    const c = portCount([
      p({ observedState: 'listening', bindAddr: '0.0.0.0' }),
      p({ observedState: 'listening', bindAddr: '127.0.0.1', loopback: true }),
      p({ observedState: 'listening', enabled: true }),
      p({ observedState: 'gone', enabled: true }),
      p({ observedState: null, declared: true }),
    ])
    expect(c).toEqual({ listening: 3, previewed: 2 })
    expect(portCountText(c)).toBe('3 listening · 2 previewed')
    expect(portCountLabel(c)).toBe('3 ports listening in the container, 2 ports previewed')
  })

  it('leaves a hidden row out of the listening count, and still counts its preview', () => {
    const shown = portCount([p({ observedState: 'listening' })])
    expect(shown.listening).toBe(1) // the control
    const hidden = portCount([p({ observedState: 'listening', hidden: true, enabled: true })])
    expect(hidden).toEqual({ listening: 0, previewed: 1 })
    expect(portCountText(hidden)).toBe('1 previewed')
  })

  it('says nothing when nothing is listening and nothing is previewed', () => {
    expect(portCountText(portCount([]))).toBeNull()
    expect(portCountText(portCount([p({ declared: true }), p({ observedState: 'gone' })]))).toBeNull()
    expect(portCountText({ listening: 1, previewed: 0 })).toBe('1 listening')
    expect(portCountLabel({ listening: 1, previewed: 1 })).toBe('1 port listening in the container, 1 port previewed')
  })
})
