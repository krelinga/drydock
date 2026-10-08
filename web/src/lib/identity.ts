// What the fleet says about the shared Claude login (frontend §6.6, design
// §7.3), as pure functions of the reducer's one identity field — so "one
// fault, one message" is a property of these functions and a parameterised
// test, not something read off ten rendered cards.
//
// Three rules from CLAUDE.md, each a sentence here:
//
//   - blanked and absent are different sentences. `auth status` reports
//     loggedIn:false for both; the server's watch told them apart by the
//     file, and the UI must not collapse them again. Blanked: everyone just
//     lost access. Absent: nobody ever had it — the expected first run.
//   - a blanked credential is not an expired one: no countdown, no "expired".
//   - expired is not a fault at all. It dates the access token, which the
//     next session server renews from the live refresh token beside it
//     (design §7.3, Spike 00), so it gets no banner, no card overlay and no
//     button; a login whose refresh token is dead is blanked, not expired.
//   - the access token's expiry is never a countdown. A real one is about
//     eight hours out (measured 2026-10-08) and every refresh moves it, so a
//     countdown on it would warn after every sign-in and teach the operator
//     to ignore the banner. Expiring counts down to `loginExpiresAt` — the
//     refresh token's end, as Claude Code records it — and an `expiring`
//     without one (a row or event stored before v0.4.5, when the word meant
//     the access token) says nothing at all.
//   - one cause, one message, one button. The banner carries the only Sign in
//     to Claude; a card shows what the login broke and carries no button for
//     it, and a card whose fault is its own (a failed build) is left alone.

import type { WorkspaceState } from '../api/types'
import type { ClaudeIdentity } from '../stores/reducer'
import { relativeTime } from './time'

/**
 * Where "Sign in to Claude" goes: the Claude section of Settings, where
 * frontend §5 puts the identity and §6.2's handshake view lives
 * (components/ClaudeLogin.vue). On that page the banner offers no link of its
 * own: the handshake's button is the one Sign in to Claude there.
 */
export const SIGN_IN_TARGET = { path: '/settings', hash: '#claude' } as const
export const SIGN_IN_LABEL = 'Sign in to Claude'

/**
 * The login's own end, when the identity is counting down to it: `expiring`
 * with a `loginExpiresAt`. Anything else — including `expiring` without one,
 * the old meaning — is null, and nothing warns.
 */
export function loginEnding(id: ClaudeIdentity | null): string | null {
  return id !== null && id.state === 'expiring' && id.loginExpiresAt !== null ? id.loginExpiresAt : null
}

export interface IdentityBanner {
  state: 'expiring' | 'blanked' | 'absent' | 'unknown'
  tone: 'warn' | 'bad'
  title: string
  body: string
  /** The one button. Null when signing in is not what fixes it. */
  action: { to: typeof SIGN_IN_TARGET; label: string } | null
  /** Only the countdown may be put away (§6.6), and only until it changes. */
  dismissible: boolean
}

const signIn = { to: SIGN_IN_TARGET, label: SIGN_IN_LABEL }

/** The fleet banner for the identity, or null when there is nothing to say. */
export function identityBanner(id: ClaudeIdentity | null, now: number = Date.now()): IdentityBanner | null {
  if (id === null) return null // not loaded: say nothing rather than guess
  switch (id.state) {
    case 'ok':
      return null
    case 'expiring': {
      const ends = loginEnding(id)
      if (ends === null) return null // the old meaning: the access token, not the login
      const when = relativeTime(ends, now)
      return {
        state: 'expiring', tone: 'warn', dismissible: true, action: signIn,
        title: `The Claude login expires ${when}.`,
        body: 'Sign in again before then. One sign-in renews every workspace.',
      }
    }
    case 'expired':
      // The access token has lapsed; the refresh token is live and the next
      // session server renews it. Nothing for the operator to do.
      return null
    case 'blanked':
      return {
        state: 'blanked', tone: 'bad', dismissible: false, action: signIn,
        title: 'Signed out. Sign in again.',
        body: 'Claude was signed out on the shared volume, so every workspace lost access at once. One sign-in fixes all of them.',
      }
    case 'absent':
      return {
        state: 'absent', tone: 'warn', dismissible: false, action: signIn,
        title: 'No one has signed in yet.',
        body: 'Sessions start once someone signs in to Claude. One sign-in covers every workspace.',
      }
    case null:
      // Never checked successfully. Not absent — Drydock has not looked —
      // and signing in is not known to be the fix, so no button.
      if (id.checkError === null) return null
      return {
        state: 'unknown', tone: 'warn', dismissible: false, action: null,
        title: 'Drydock could not check the Claude login yet.',
        body: id.checkError.message,
      }
  }
}

export interface CardOverlay {
  kind: 'dot' | 'waiting'
  text: string
}

/**
 * What the identity does to one workspace's card. Only a running workspace
 * runs a session, so only its card changes: a warning dot while the login
 * itself is ending (never for the access token's hours), and "waiting on
 * Claude sign-in" in place of the session once it cannot run. No button —
 * the banner holds the one that works (§6.6). A stopped, failed or building
 * card keeps its own status: its fault, if it has one, is not the login's.
 */
export function cardOverlay(id: ClaudeIdentity | null, state: WorkspaceState | null): CardOverlay | null {
  if (id === null || state !== 'running') return null
  switch (id.state) {
    case 'expiring':
      return loginEnding(id) !== null ? { kind: 'dot', text: 'Claude login expiring' } : null
    case 'blanked':
    case 'absent':
      return { kind: 'waiting', text: 'Waiting on Claude sign-in.' }
  }
  return null
}

function signedIn(id: ClaudeIdentity): string {
  return id.accountEmail !== null ? `Signed in as ${id.accountEmail}.` : 'Signed in.'
}

/** The Settings section's one-line state. Same distinctions as the banner. */
export function identitySentence(id: ClaudeIdentity | null): string {
  if (id === null) return 'Checking…'
  switch (id.state) {
    case 'ok':
      return signedIn(id)
    case 'expiring': {
      const ends = loginEnding(id)
      // The old meaning (no loginExpiresAt) was the access token: say ok.
      return ends === null ? signedIn(id) : `Signed in. The login expires ${relativeTime(ends)}; sign in again before then.`
    }
    case 'expired':
      return 'Signed in. The access token has lapsed; the next session server to start renews it.'
    case 'blanked':
      return 'Signed out. Sign in again.'
    case 'absent':
      return 'No one has signed in yet.'
  }
  return 'Not checked yet.'
}
