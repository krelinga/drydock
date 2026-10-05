// The reducer's secret cases (frontend §4.1, §10). Each frame's `data` is
// what internal/secrets' emit calls marshal. As in reducer.spec.ts, every
// negative case keeps a positive control in the same test.

import { describe, expect, it } from 'vitest'
import type { SecretMeta, StreamEvent } from '../api/types'
import { emptyEntities, reduce, reduceAll, secretList, type Action, type Entities } from './reducer'

const at = (n: number) => new Date(Date.UTC(2026, 9, 5, 9, 0, n)).toISOString()

function meta(name: string, over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    name, reach: `What ${name} reaches.`, description: '', all_repos: false, grants: [],
    created_at: at(0), rotated_at: null, last_access_at: null, accessed_by: [], ...over,
  }
}

function ev(id: number, kind: string, data?: Record<string, unknown>, message = kind, level: StreamEvent['level'] = 'info'): StreamEvent {
  return { id, kind, message, level, at: at(id), ...(data !== undefined ? { data } : {}) }
}

const events = (evs: StreamEvent[]): Action[] => evs.map((event) => ({ type: 'event', event }))
const play = (evs: StreamEvent[], start: Entities = emptyEntities()) => reduceAll(start, events(evs))

describe('secret events', () => {
  it('create, grant, rotate and delete, in order', () => {
    const steps: StreamEvent[] = [
      ev(1, 'secret.created', { secret: meta('API_KEY') }),
      ev(2, 'secret.grants', { secret: meta('API_KEY', { grants: [{ repository_id: 1, full_name: 'krelinga/drydock' }] }) }),
      ev(3, 'secret.rotated', { secret: meta('API_KEY', { rotated_at: at(3), grants: [{ repository_id: 1, full_name: 'krelinga/drydock' }] }), stale: { new_commands: [], needs_supervisor_restart: [] } }),
    ]
    let e = play(steps)
    expect(e.secrets.API_KEY).toMatchObject({
      name: 'API_KEY', rotatedAt: at(3), grants: [{ repositoryId: 1, fullName: 'krelinga/drydock' }], at: 3,
    })
    expect(e.secretsWrittenAt).toBe(3)
    e = reduce(e, { type: 'event', event: ev(4, 'secret.deleted', { name: 'API_KEY' }) })
    expect(e.secrets.API_KEY).toBeUndefined()
    expect(e.secretsDeleted.API_KEY).toBe(4)
    // Secret events name no workspace and touch none.
    expect(e.workspaces).toEqual({})
    expect(e.lastEventId).toBe(4)
  })

  it('an older event cannot roll a secret back, and a newer one can move it', () => {
    const newer = ev(5, 'secret.grants', { secret: meta('K', { all_repos: true }) })
    const older = ev(4, 'secret.created', { secret: meta('K') })
    const e = play([newer, older])
    expect(e.secrets.K!.allRepos).toBe(true)
    // Control: in order, the newer one still wins.
    expect(play([older, newer]).secrets.K!.allRepos).toBe(true)
    expect(play([older]).secrets.K!.allRepos).toBe(false)
  })

  it('a name can come back after a delete, but a stale event cannot bring it back', () => {
    const create = ev(1, 'secret.created', { secret: meta('K') })
    const del = ev(2, 'secret.deleted', { name: 'K' })
    const late = ev(1, 'secret.created', { secret: meta('K') }) // a replayed duplicate
    expect(play([create, del, late]).secrets.K).toBeUndefined()
    // Control: a genuinely new create after the delete recreates it.
    const again = ev(3, 'secret.created', { secret: meta('K', { reach: 'Second life.' }) })
    expect(play([create, del, again]).secrets.K?.reach).toBe('Second life.')
  })

  it('copies named fields only: a value the server never sends could not reach the store', () => {
    const leaky = { ...meta('K'), value: 'CANARY-not-a-field', ciphertext: 'x' }
    const e = play([ev(1, 'secret.created', { secret: leaky })])
    expect(JSON.stringify(e)).not.toContain('CANARY-not-a-field')
    expect(Object.keys(e.secrets.K!).sort()).toEqual([
      'accessedBy', 'allRepos', 'at', 'createdAt', 'description', 'grants', 'lastAccessAt', 'name', 'reach', 'rotatedAt',
    ])
    // Control: the metadata itself arrived.
    expect(e.secrets.K!.reach).toBe('What K reaches.')
  })

  it('does not keep a rotation\'s stale list: it is the PUT\'s result, not the secret\'s state', () => {
    const stale = { new_commands: [{ workspace_id: 'W1', repository_id: 1, full_name: 'krelinga/drydock' }], needs_supervisor_restart: [] }
    const e = play([ev(1, 'secret.rotated', { secret: meta('K', { rotated_at: at(1) }), stale })])
    expect(JSON.stringify(e)).not.toContain('W1')
    expect(e.secrets.K!.rotatedAt).toBe(at(1))
  })

  it('ignores a malformed frame but still advances the stream position', () => {
    const e = play([ev(1, 'secret.created', { secret: { reach: 'no name' } }), ev(2, 'secret.deleted', {})])
    expect(e.secrets).toEqual({})
    expect(e.lastEventId).toBe(2)
    // Control: a well-formed frame after them lands.
    expect(reduce(e, { type: 'event', event: ev(3, 'secret.created', { secret: meta('K') }) }).secrets.K).toBeDefined()
  })

  it('records secret.undeliverable as the fleet fault, newest wins', () => {
    const e = play([
      ev(1, 'secret.undeliverable', undefined, 'first', 'error'),
      ev(3, 'secret.undeliverable', undefined, 'second', 'error'),
      ev(2, 'secret.undeliverable', undefined, 'late', 'error'),
    ])
    expect(e.secretFault).toEqual({ at: at(3), message: 'second', eventId: 3 })
    // A write after it is what the banner uses to say the report is older.
    expect(e.secretsWrittenAt).toBe(0)
    expect(reduce(e, { type: 'event', event: ev(4, 'secret.created', { secret: meta('K') }) }).secretsWrittenAt).toBe(4)
    expect(emptyEntities().secretFault).toBeNull()
  })
})

describe('the GET /api/secrets snapshot', () => {
  it('loads every secret, and the list is sorted by name', () => {
    const e = reduce(emptyEntities(), { type: 'secrets', at: 0, view: { secrets: [meta('ZED'), meta('ALPHA')] } })
    expect(e.secretsLoaded).toBe(true)
    expect(secretList(e).map((s) => s.name)).toEqual(['ALPHA', 'ZED'])
    // Control: before any snapshot, "not loaded", not "none".
    expect(emptyEntities().secretsLoaded).toBe(false)
  })

  it('is the authority on existence, except for what an event after it wrote', () => {
    const before = play([ev(1, 'secret.created', { secret: meta('OLD') }), ev(5, 'secret.created', { secret: meta('NEW') })])
    // Taken at 3: it knows nothing of NEW (created at 5), and OLD is gone from the server.
    const e = reduce(before, { type: 'secrets', at: 3, view: { secrets: [] } })
    expect(e.secrets.OLD).toBeUndefined()
    expect(e.secrets.NEW).toBeDefined()
  })

  it('keeps an event newer than it, and takes its own over anything older', () => {
    const e0 = play([ev(1, 'secret.created', { secret: meta('K', { reach: 'v1' }) }), ev(6, 'secret.created', { secret: meta('J', { reach: 'event' }) })])
    const e = reduce(e0, {
      type: 'secrets', at: 4,
      view: { secrets: [meta('K', { reach: 'snapshot', last_access_at: at(4), accessed_by: ['W1'] }), meta('J', { reach: 'snapshot' })] },
    })
    expect(e.secrets.K).toMatchObject({ reach: 'snapshot', lastAccessAt: at(4), accessedBy: ['W1'], at: 4 })
    expect(e.secrets.J!.reach).toBe('event')
  })

  it('cannot resurrect a secret deleted after it was taken', () => {
    const e0 = play([ev(1, 'secret.created', { secret: meta('K') }), ev(7, 'secret.deleted', { name: 'K' })])
    expect(reduce(e0, { type: 'secrets', at: 5, view: { secrets: [meta('K')] } }).secrets.K).toBeUndefined()
    // Control: a snapshot taken after the delete that lists it again — a
    // recreate whose event we missed — is believed.
    expect(reduce(e0, { type: 'secrets', at: 9, view: { secrets: [meta('K')] } }).secrets.K).toBeDefined()
  })
})
