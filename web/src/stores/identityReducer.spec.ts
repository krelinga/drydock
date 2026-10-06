// The reducer's Claude identity cases (frontend §4.1, §6.6). Each frame's
// `data` is what internal/identity's Emit calls marshal; the snapshot is a
// GET /api/auth/claude body. Every negative case keeps a positive control in
// the same test.

import { describe, expect, it } from 'vitest'
import type { ClaudeIdentityBody, IdentityView, StreamEvent } from '../api/types'
import { emptyEntities, reduce, reduceAll, type Action } from './reducer'

const at = (n: number) => new Date(Date.UTC(2026, 9, 5, 9, 0, n)).toISOString()

function view(state: IdentityView['state'], over: Partial<IdentityView> = {}): IdentityView {
  const live = state === 'ok' || state === 'expiring' || state === 'expired'
  return {
    state,
    account_email: live ? 'fixture@example.invalid' : null,
    expires_at: live ? '2026-10-07T00:00:00Z' : null,
    logged_in_at: live ? at(0) : null,
    last_checked_at: at(1),
    volume: 'drydock-claude-config',
    check_error: null,
    ...over,
  }
}

const body = (v: IdentityView): ClaudeIdentityBody => ({ identity: v, login: null })

function ev(id: number, kind: string, data: Record<string, unknown>, message = kind): StreamEvent {
  return { id, kind, message, level: 'info', at: at(id), data }
}
const identityEv = (id: number, v: IdentityView) => ev(id, 'auth.identity', { identity: v })
const failedEv = (id: number) => ev(id, 'auth.identity_check_failed',
  { check_error: { at: at(id), problem: 'docker', message: 'Could not check the Claude login: Docker did not answer. The last known state is kept.' } })

const play = (actions: Action[]) => reduceAll(emptyEntities(), actions)

describe('the Claude identity', () => {
  it('is not loaded until a snapshot or an event says something', () => {
    expect(emptyEntities().identity).toBeNull()
    const e = play([{ type: 'identity', at: 4, view: body(view('blanked')) }])
    expect(e.identity).toMatchObject({ state: 'blanked', accountEmail: null, volume: 'drydock-claude-config' })
    expect(e.identityAt).toBe(4)
    expect(e.lastEventId).toBe(4)
  })

  it('blanked and absent stay distinct through the reducer', () => {
    // The trap: both are loggedIn:false to `auth status`. The server told
    // them apart; the reducer must not fold them together again.
    const blanked = play([{ type: 'event', event: identityEv(1, view('blanked')) }])
    const absent = play([{ type: 'event', event: identityEv(1, view('absent')) }])
    expect(blanked.identity?.state).toBe('blanked')
    expect(absent.identity?.state).toBe('absent')
  })

  it('an event newer than the snapshot wins, and an older one does not', () => {
    // Snapshot at 5 says ok; the event at 7 says blanked: blanked stands.
    let e = play([
      { type: 'event', event: identityEv(7, view('blanked')) },
      { type: 'identity', at: 5, view: body(view('ok')) },
    ])
    expect(e.identity?.state).toBe('blanked')
    // A straggler from before the snapshot cannot roll it back.
    e = play([
      { type: 'identity', at: 9, view: body(view('expiring')) },
      { type: 'event', event: identityEv(8, view('ok')) },
    ])
    expect(e.identity?.state).toBe('expiring')
    // Control: a later event does move it.
    e = reduce(e, { type: 'event', event: identityEv(10, view('expired')) })
    expect(e.identity?.state).toBe('expired')
  })

  it('a failed check keeps the state and records the failure; the next verdict clears it', () => {
    let e = play([{ type: 'event', event: identityEv(1, view('expiring')) }, { type: 'event', event: failedEv(2) }])
    expect(e.identity?.state).toBe('expiring')
    expect(e.identity?.accountEmail).toBe('fixture@example.invalid')
    expect(e.identity?.checkError?.message).toContain('The last known state is kept.')
    expect(e.identity?.lastCheckedAt).toBe(at(2))
    e = reduce(e, { type: 'event', event: identityEv(3, view('expiring')) })
    expect(e.identity?.checkError).toBeNull()
  })

  it('a failure before any verdict is "not known", never absent', () => {
    const e = play([{ type: 'event', event: failedEv(1) }])
    expect(e.identity?.state).toBeNull()
    expect(e.identity?.checkError).not.toBeNull()
  })

  it('a state outside the five is not known, never ok', () => {
    const e = play([{ type: 'event', event: identityEv(1, { ...view('ok'), state: 'signed_out' as never }) }])
    expect(e.identity?.state).toBeNull()
    // Control: the same event with a real state is applied as that state.
    expect(play([{ type: 'event', event: identityEv(1, view('ok')) }]).identity?.state).toBe('ok')
  })

  it('takes named fields only', () => {
    const leaky = { ...view('ok'), accessToken: 'sk-ant-oat01-NOT-HERE' } as unknown as IdentityView
    const e = play([{ type: 'identity', at: 1, view: body(leaky) }])
    expect(JSON.stringify(e)).not.toContain('sk-ant-oat01-NOT-HERE')
    expect(e.identity?.state).toBe('ok')
  })

  it('names no workspace and touches none', () => {
    const e = play([{ type: 'event', event: identityEv(1, view('blanked')) }])
    expect(e.workspaces).toEqual({})
    expect(e.feeds).toEqual({})
  })
})
