// §6.1's state table, the supervisor half (Phase 5), and §6.6's fleet
// override over it — as a parameterized test of the pure mapping: every
// (running, supervisor.state) pair renders one status line and at most one
// action, read from the supervisor entity and never from workspace.state.

import { describe, expect, it } from 'vitest'
import type { StreamEvent } from '../api/types'
import { emptyEntities, environmentURL, reduce, reduceAll, type Workspace } from '../stores/reducer'
import {
  CLONE_OK, ENV, SERVE_OK, WS, listBody, sessionEvent, stateEvent, stepEvent, supEvent, wsView,
} from '../stores/reducer.fixtures'
import { cardStatus, stoppable, type FleetLogin } from './workspaceCard'

const run = (evs: StreamEvent[]): Workspace =>
  reduceAll(emptyEntities(), evs.map((event) => ({ type: 'event' as const, event }))).workspaces[WS]!

const serving = () => run([...CLONE_OK, ...SERVE_OK])
const withSup = (state: string, reason: string, detail = 'Drydock’s sentence.') =>
  run([...CLONE_OK, ...SERVE_OK, supEvent(30, state, reason, detail)])

describe('the card, supervisor half (§6.1)', () => {
  it.each([
    ['starting', 'launching', 'Starting session…', 'busy', null],
    ['awaiting_login', 'no_organization', 'Claude is not signed in', 'bad', null],
    ['waiting_registration', 'wait_registration', 'Waiting for the previous session server to release the folder', 'busy', null],
    ['degraded', 'budget_spent', 'Session degraded', 'bad', 'restart_session'],
    ['degraded', 'not_trusted', 'Container misconfigured', 'bad', 'rebuild'],
    ['degraded', 'hang_remote_dialog', 'Container misconfigured', 'bad', 'rebuild'],
    ['degraded', 'hang_trust', 'Container misconfigured', 'bad', 'rebuild'],
    // A container an earlier Drydock made, with the broker socket mounted as a
    // file: no broker since the restart, and only a rebuild fixes it.
    ['degraded', 'stale_broker_mount', 'Container misconfigured', 'bad', 'rebuild'],
    ['degraded', 'bad_command_line', 'Drydock built a bad command line', 'bad', null],
    ['exited', 'stopped', 'Session stopped', 'idle', 'start_session'],
  ])('%s (%s) → "%s", %s, action %s', (state, reason, line, tone, action) => {
    expect(cardStatus(withSup(state, reason))).toMatchObject({ line, tone, action })
  })

  it('serving shows the capacity fraction and the environment link, and is the only row with one', () => {
    const s = cardStatus(serving())
    expect(s).toMatchObject({ line: 'Capacity 1 / 4', tone: 'ok', action: 'open', link: `https://claude.ai/code?environment=${ENV}` })
    // Control: no other row carries a link.
    for (const [st, r] of [['starting', 'launching'], ['degraded', 'budget_spent'], ['waiting_registration', 'wait_registration']]) {
      expect(cardStatus(withSup(st!, r!)).link ?? null).toBeNull()
    }
  })

  it('the capacity counts the pre-created session: 1 / 4 is what the server printed, never "3 sessions"', () => {
    const s = cardStatus(run([...CLONE_OK, ...SERVE_OK, sessionEvent(31, 2, 4)]))
    expect(s.line).toBe('Capacity 2 / 4')
    expect(s.line).not.toMatch(/session/i)
  })

  it('a step 8 boot closed after a crash does not override the server it then restarted', () => {
    // Design §6: Drydock died inside step 8; the next boot failed the step
    // with its sentence and started the session server again. The card is
    // the supervisor's, as for any running workspace; the timeline keeps the
    // closed step.
    const closed = 'The session server step failed: Drydock stopped while this step was running.'
    const s = cardStatus(run([
      ...CLONE_OK, stepEvent(14, WS, 'session_server', 'started'), stepEvent(20, WS, 'session_server', 'failed', closed),
      supEvent(21, 'starting', 'launching', 'Starting the session server.'), supEvent(22, 'serving', 'connected', '', 'starting'),
      sessionEvent(23, 1, 4),
    ]))
    expect(s).toMatchObject({ line: 'Capacity 1 / 4', tone: 'ok', action: 'open' })
    // Control: the same close with no supervisor event after it still reads running, never failed.
    const bare = cardStatus(run([...CLONE_OK, stepEvent(14, WS, 'session_server', 'started'),
      stepEvent(20, WS, 'session_server', 'failed', closed)]))
    expect(bare.line).not.toMatch(/fail/i)
  })

  it('waiting_registration is a wait: no action, elapsed time, never the word failed', () => {
    const s = cardStatus(withSup('waiting_registration', 'wait_registration'))
    expect(s.action).toBeNull()
    expect(s.since).toBeTruthy()
    expect(`${s.line} ${s.note}`).not.toMatch(/fail/i)
  })

  it('a running workspace whose supervisor never reported says so and offers a start', () => {
    const w = reduce(emptyEntities(), { type: 'workspaces', at: 30, view: listBody(wsView({ supervisor: null, session: null })) }).workspaces[WS]!
    expect(cardStatus(w)).toMatchObject({ line: 'Container up, no session', action: 'start_session' })
  })

  it('a server older than Phase 5 (no supervisor field) keeps the workspace half: Running, Stop', () => {
    const w = reduce(emptyEntities(), { type: 'workspaces', at: 30, view: listBody(wsView()) }).workspaces[WS]!
    expect(cardStatus(w)).toMatchObject({ line: 'Running', action: 'stop' })
  })

  it('reads the supervisor entity, not workspace.state: a running move alone changes nothing about the session', () => {
    const w = run([...CLONE_OK, ...SERVE_OK, supEvent(30, 'degraded', 'budget_spent'), stateEvent(31, WS, 'running')])
    expect(cardStatus(w).line).toBe('Session degraded')
  })

  it('the link is built from an environment id, never taken from the wire', () => {
    const evil = { ...sessionEvent(31, 1, 4), data: { environment_id: 'javascript:alert(1)', url: 'javascript:alert(1)', capacity_used: 1, capacity_total: 4, sessions: 1 } }
    const s = cardStatus(run([...CLONE_OK, ...SERVE_OK, evil]))
    expect(s.link ?? null).toBeNull()
    expect(s.action).toBeNull()
    expect(environmentURL(ENV)).toBe(`https://claude.ai/code?environment=${ENV}`)
  })

  it('a running workspace stays stoppable whatever its card action is', () => {
    expect(stoppable(serving())).toBe(true)
    expect(stoppable(withSup('degraded', 'budget_spent'))).toBe(true)
  })
})

describe('one fault, ten cards (§6.6)', () => {
  const signedOut: FleetLogin[] = ['blanked', 'absent']

  it.each(signedOut)('%s: every session-dependent card drops its session line and button (the waiting sentence is the identity note’s)', (fleet) => {
    const cards = [
      serving(), withSup('degraded', 'budget_spent'), withSup('awaiting_login', 'signed_out'),
      withSup('starting', 'backoff'), withSup('exited', 'stopped'),
    ].map((w) => cardStatus(w, fleet))
    for (const c of cards) { expect(c).toMatchObject({ line: 'Running', note: null, action: null }); expect(c.link ?? null).toBeNull() }
    // No Restart session server anywhere: that button cannot work.
    expect(cards.filter((c) => c.action === 'restart_session')).toHaveLength(0)
  })

  it.each(signedOut)('%s: a card whose fault is not the login’s keeps its own status and action', (fleet) => {
    expect(cardStatus(withSup('degraded', 'not_trusted'), fleet)).toMatchObject({ line: 'Container misconfigured', action: 'rebuild' })
    expect(cardStatus(withSup('degraded', 'stale_broker_mount'), fleet)).toMatchObject({ line: 'Container misconfigured', action: 'rebuild' })
    expect(cardStatus(withSup('degraded', 'bad_command_line'), fleet).line).toBe('Drydock built a bad command line')
  })

  it('the control: ok, expiring and expired leave every card its own row', () => {
    // Expired is the access token's lapse, which a starting server renews:
    // the supervisor starts servers under it, so the cards say what they do.
    for (const fleet of ['ok', 'expiring', 'expired', null] as FleetLogin[]) {
      expect(cardStatus(withSup('degraded', 'budget_spent'), fleet).action).toBe('restart_session')
      expect(cardStatus(serving(), fleet).action).toBe('open')
    }
  })
})
