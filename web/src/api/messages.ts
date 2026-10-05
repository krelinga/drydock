// Error code → the sentence the UI shows (frontend §9).
//
// This is a lookup on the envelope's `code`, never a match on the server's
// prose. The server's `message` is written for a human too, but a reworded
// message must not change what the UI says, and a message the server never
// meant for this screen must not leak onto it. So the server's text is not an
// input here at all.

import { ApiError } from './client'

const SENTENCES: Record<string, string> = {
  unauthenticated: 'Your session has ended. Sign in again.',
  bad_password: 'That password is not right.',
  not_configured: 'Drydock has no password yet. Run `drydock passwd` on the host.',
  app_not_configured: 'No GitHub App is set up yet, so there are no repositories to show.',
  forbidden_origin: "This request did not come from Drydock's own page. Reload and try again.",
  forbidden_host: 'This page was not reached at Drydock’s own address.',
  not_found: 'Drydock has no such thing.',
  method_not_allowed: 'This page asked Drydock for something the wrong way. Reload and try again.',
  not_implemented: 'That is not built yet.',
  in_progress: 'Already in progress.',
  // Design §1: capacity is managed by hand, so the refusal says where the
  // workspaces holding the slots are (frontend §9). It promises no button:
  // there is no Stop until Phase 6. REVISIT when Stop exists — name the cap's
  // value and point at the Stop button on the cards under Running.
  at_capacity: 'Drydock is at its cap: as many workspaces as it allows are already building or running. They are listed under Running.',
  bad_request: 'Drydock did not understand that request.',
  internal: 'Drydock hit an internal error. The host’s log has the detail.',
  network: 'Could not reach Drydock. Check the connection and try again.',
}

/** `in 45 seconds`, `in 2 minutes` — rounded up, never "in 0". */
export function inDuration(seconds: number): string {
  if (seconds < 60) {
    const s = Math.max(1, Math.ceil(seconds))
    return `in ${s} second${s === 1 ? '' : 's'}`
  }
  const m = Math.ceil(seconds / 60)
  return `in ${m} minute${m === 1 ? '' : 's'}`
}

/** What to tell the operator about a failed request. */
export function describeError(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return 'Something went wrong in the page itself. Reload and try again.'
  }
  if (err.code === 'locked_out') {
    const when = err.retryAfter !== null ? `Try again ${inDuration(err.retryAfter)}.` : 'Wait a while before trying again.'
    return `Too many failed sign-ins. ${when}`
  }
  const sentence = SENTENCES[err.code]
  if (sentence !== undefined) return sentence
  // An unknown code is rendered as one, visibly, rather than guessed at:
  // the code is what someone will search for.
  return `Something went wrong (${err.status || 'no response'}, ${err.code}).`
}
