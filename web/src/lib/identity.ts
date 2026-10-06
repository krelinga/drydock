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
//   - one cause, one message, one button. The banner carries the only Sign in
//     to Claude; a card shows what the login broke and carries no button for
//     it, and a card whose fault is its own (a failed build) is left alone.

import type { WorkspaceState } from '../api/types'
import type { ClaudeIdentity } from '../stores/reducer'
import { relativeTime } from './time'

/**
 * Where "Sign in to Claude" goes: the Claude section of Settings, where
 * frontend §5 puts the identity and §6.2's handshake view will live.
 *
 * SEAM (Phase 5, the login handshake): until POST /api/auth/claude/login is
 * built, that section says plainly that signing in from Drydock comes next,
 * and offers nothing that pretends to sign in. When it lands, this link is
 * still right — the handshake view mounts there — and only Settings changes.
 */
export const SIGN_IN_TARGET = { path: '/settings', hash: '#claude' } as const
export const SIGN_IN_LABEL = 'Sign in to Claude'

export interface IdentityBanner {
  state: 'expiring' | 'expired' | 'blanked' | 'absent' | 'unknown'
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
      const when = id.expiresAt !== null ? relativeTime(id.expiresAt, now) : 'soon'
      return {
        state: 'expiring', tone: 'warn', dismissible: true, action: signIn,
        title: `The Claude login expires ${when}.`,
        body: 'Sign in again before then. One sign-in renews every workspace.',
      }
    }
    case 'expired':
      return {
        state: 'expired', tone: 'bad', dismissible: false, action: signIn,
        title: 'The Claude login has expired. Sign in again.',
        body: 'Session servers cannot run until someone signs in. One sign-in fixes every workspace.',
      }
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
 * runs a session, so only its card changes: a warning dot while the login is
 * expiring, and "waiting on Claude sign-in" in place of the session once it
 * cannot run. No button — the banner holds the one that works (§6.6). A
 * stopped, failed or building card keeps its own status: its fault, if it has
 * one, is not the login's.
 */
export function cardOverlay(id: ClaudeIdentity | null, state: WorkspaceState | null): CardOverlay | null {
  if (id === null || state !== 'running') return null
  switch (id.state) {
    case 'expiring':
      return { kind: 'dot', text: 'Claude login expiring' }
    case 'expired':
    case 'blanked':
    case 'absent':
      return { kind: 'waiting', text: 'Waiting on Claude sign-in.' }
  }
  return null
}

/** The Settings section's one-line state. Same distinctions as the banner. */
export function identitySentence(id: ClaudeIdentity | null): string {
  if (id === null) return 'Checking…'
  switch (id.state) {
    case 'ok':
      return id.accountEmail !== null ? `Signed in as ${id.accountEmail}.` : 'Signed in.'
    case 'expiring':
      return 'Signed in, and the login expires soon.'
    case 'expired':
      return 'The login has expired.'
    case 'blanked':
      return 'Signed out. Sign in again.'
    case 'absent':
      return 'No one has signed in yet.'
  }
  return 'Not checked yet.'
}
