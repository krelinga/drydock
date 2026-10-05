// The identity's words (frontend §6.6, design §7.3) as a parameterised table:
// one sentence per state, blanked and absent never alike, a countdown only for
// expiring, and at most one button — the banner's — for any of them.

import { describe, expect, it } from 'vitest'
import type { IdentityState } from '../api/types'
import type { ClaudeIdentity } from '../stores/reducer'
import { cardOverlay, identityBanner, identitySentence, SIGN_IN_LABEL } from './identity'

const NOW = Date.parse('2026-10-05T09:00:00Z')

function id(state: IdentityState | null, over: Partial<ClaudeIdentity> = {}): ClaudeIdentity {
  return {
    state, accountEmail: null, expiresAt: null, loggedInAt: null, lastCheckedAt: null,
    volume: 'drydock-claude-config', checkError: null, ...over,
  }
}

describe('the identity banner', () => {
  const cases: Array<[IdentityState, string, 'warn' | 'bad', boolean]> = [
    ['blanked', 'Signed out. Sign in again.', 'bad', false],
    ['absent', 'No one has signed in yet.', 'warn', false],
    ['expired', 'The Claude login has expired. Sign in again.', 'bad', false],
  ]
  it.each(cases)('%s says %j', (state, title, tone, dismissible) => {
    const b = identityBanner(id(state), NOW)
    expect(b?.title).toBe(title)
    expect(b?.tone).toBe(tone)
    expect(b?.dismissible).toBe(dismissible)
    expect(b?.action?.label).toBe(SIGN_IN_LABEL)
  })

  it('blanked and absent are different sentences', () => {
    const blanked = identityBanner(id('blanked'), NOW)!
    const absent = identityBanner(id('absent'), NOW)!
    expect(blanked.title).not.toBe(absent.title)
    expect(blanked.body).not.toBe(absent.body)
    expect(blanked.body).toMatch(/every workspace lost access/)
    expect(absent.body).not.toMatch(/lost/)
  })

  it('a blanked login is never called expired, and carries no countdown', () => {
    const b = identityBanner(id('blanked', { expiresAt: '2026-10-06T09:00:00Z' }), NOW)!
    expect(`${b.title} ${b.body}`).not.toMatch(/expire/i)
    expect(`${b.title} ${b.body}`).not.toMatch(/\bin \d/)
  })

  it('expiring counts down and may be put away', () => {
    const b = identityBanner(id('expiring', { expiresAt: '2026-10-07T09:00:00Z' }), NOW)!
    expect(b.title).toBe('The Claude login expires in 2 days.')
    expect(b.dismissible).toBe(true)
    expect(b.tone).toBe('warn')
  })

  it('ok and not-loaded say nothing', () => {
    expect(identityBanner(id('ok'), NOW)).toBeNull()
    expect(identityBanner(null, NOW)).toBeNull()
  })

  it('never checked: says so without guessing, and offers no sign-in', () => {
    expect(identityBanner(id(null), NOW)).toBeNull()
    const b = identityBanner(id(null, { checkError: { at: '', problem: 'docker', message: 'Docker did not answer.' } }), NOW)!
    expect(b.title).toBe('Drydock could not check the Claude login yet.')
    expect(b.body).toBe('Docker did not answer.')
    expect(b.action).toBeNull()
    expect(b.title).not.toMatch(/signed in yet/)
  })
})

describe('the card overlay', () => {
  it('changes only a running card', () => {
    for (const s of ['pending', 'cloning', 'building', 'stopped', 'failed', 'deleting', null] as const) {
      expect(cardOverlay(id('blanked'), s)).toBeNull()
    }
    // Control: the same identity does change a running card.
    expect(cardOverlay(id('blanked'), 'running')).toEqual({ kind: 'waiting', text: 'Waiting on Claude sign-in.' })
  })

  it('a dot while expiring, the waiting line once a session cannot run, nothing when ok', () => {
    expect(cardOverlay(id('expiring'), 'running')?.kind).toBe('dot')
    expect(cardOverlay(id('expired'), 'running')?.kind).toBe('waiting')
    expect(cardOverlay(id('absent'), 'running')?.kind).toBe('waiting')
    expect(cardOverlay(id('ok'), 'running')).toBeNull()
    expect(cardOverlay(null, 'running')).toBeNull()
  })
})

describe('the settings sentence', () => {
  it('keeps the same distinctions', () => {
    const all = (['ok', 'expiring', 'expired', 'blanked', 'absent'] as const).map((s) => identitySentence(id(s)))
    expect(new Set(all).size).toBe(5)
    expect(identitySentence(id('blanked'))).toBe('Signed out. Sign in again.')
    expect(identitySentence(id('absent'))).toBe('No one has signed in yet.')
    expect(identitySentence(id('ok', { accountEmail: 'a@b.invalid' }))).toBe('Signed in as a@b.invalid.')
  })
})
