// Error code → the sentence the UI shows (frontend §9).
//
// This is a lookup on the envelope's `code`, never a match on the server's
// prose. The server's `message` is written for a human too, but a reworded
// message must not change what the UI says, and a message the server never
// meant for this screen must not leak onto it. So the server's `message` is
// not an input here at all, and its `detail` only for the codes in DETAILED.

import { ApiError } from './client'

const SENTENCES: Record<string, string> = {
  unauthenticated: 'Your session has ended. Sign in again.',
  bad_password: 'That password is not right.',
  not_configured: 'Drydock has no password yet. Run `drydock passwd` on the host.',
  // One sentence for every route that answers it — the catalog and its
  // refresh, and a clone, start or rebuild — so it names what the App is for
  // rather than what this screen was about to show.
  app_not_configured:
    'No GitHub App is set up yet, and Drydock needs one to list repositories and to clone, start or rebuild a workspace.',
  forbidden_origin: "This request did not come from Drydock's own page. Reload and try again.",
  forbidden_host: 'This page was not reached at Drydock’s own address.',
  not_found: 'Drydock has no such thing.',
  method_not_allowed: 'This page asked Drydock for something the wrong way. Reload and try again.',
  not_implemented: 'That is not built yet.',
  in_progress: 'Already in progress.',
  // Design §1: capacity is managed by hand, so the refusal says where the
  // workspaces holding the slots are and what frees one (frontend §9): Stop,
  // on the cards under Running, and that stopping costs nothing it cannot
  // bring back. The server's detail names the cap ("The cap is 10."), so it
  // is in DETAILED. The page replaces the buttons that would get this at the
  // cap it knows of, so this is reached when its count was behind.
  at_capacity:
    'Drydock is at its cap: as many workspaces as it allows are already building or running. Stop one under Running to make room — its clone survives, and Start brings it back.',
  // Design §5: a delete's ?confirm= is compared exactly. The sheet keeps its
  // button off until the text matches, so this is reached only when the name
  // changed under it (a renamed repository) or another client sent it.
  confirm_mismatch:
    "That is not the repository's full name exactly as written, so nothing was deleted. Type it again, capitals and all.",
  bad_request: 'Drydock did not understand that request.',
  // Design §10.1's refusals. Three of them need the server's detail to be
  // specific — which reason a name is reserved for, which character at which
  // byte, which repository id — and DETAILED below says which.
  secrets_not_configured:
    'Drydock has no secrets key, so secrets cannot be stored. Start `drydock serve` with `--secrets-key`; the installer creates the key.',
  secret_name_invalid:
    "A secret's name is its environment variable name: capital letters, digits and underscores, not starting with a digit, at most 128 characters.",
  secret_name_reserved: 'That name is reserved.',
  secret_value_required: 'A new secret needs a value. There is no stored value to keep.',
  secret_value_empty: 'A secret needs a value. An empty value cannot be told apart from an unset variable.',
  secret_value_control_character: "A secret's value must be a single line with no control characters.",
  secret_value_too_long:
    'That value is too long: at most 32768 bytes. Encode a large or multi-line credential, such as a PEM, as base64.',
  secret_reach_required: 'Say what someone could do with this secret. The answer is required.',
  secret_reach_too_long: 'The answer to "what can someone do with this?" is too long: at most 2000 bytes.',
  secret_description_too_long: 'The description is too long: at most 4000 bytes.',
  secret_description_invalid: 'The description must be text.',
  unknown_repository: 'That repository is not in the catalog.',
  internal: 'Drydock hit an internal error. The host’s log has the detail.',
  network: 'Could not reach Drydock. Check the connection and try again.',
}

/**
 * Codes whose sentence is completed by the envelope's `detail`. The detail is
 * the one piece of server text the UI shows, and only for these: it names the
 * rule or the character (internal/secrets.Invalid — "never the value"), or the
 * cap (internal/api writeProvisionError), which is what makes the sentence
 * worth reading. It is rendered as text (§8).
 */
const DETAILED = new Set(['secret_name_reserved', 'secret_value_control_character', 'unknown_repository', 'at_capacity'])

/**
 * The sentence for a code and its detail. A refusal the form caught before
 * sending (lib/secretRules.ts) comes through here too, with the same code and
 * detail the server would have sent, so it reads the same either way.
 */
export function sentenceFor(code: string, detail = ''): string | undefined {
  const sentence = SENTENCES[code]
  if (sentence === undefined) return undefined
  return DETAILED.has(code) && detail !== '' ? `${sentence} ${detail}` : sentence
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
  const sentence = sentenceFor(err.code, err.detail)
  if (sentence !== undefined) return sentence
  // An unknown code is rendered as one, visibly, rather than guessed at:
  // the code is what someone will search for.
  return `Something went wrong (${err.status || 'no response'}, ${err.code}).`
}
