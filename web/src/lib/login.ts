// The login handshake's client-side rules (design §7.2, frontend §6.2), as
// pure functions.
//
// The code's shape is checked here before it is sent, because the likeliest
// mistake is half a copy and the place the operator should find out is the
// field, not a round trip (§6.2). The server checks it again
// (classify.ValidateCodeShape) and is the authority; this is a copy of its
// rules, and `login.spec.ts` reads internal/classify/login.go so the two
// cannot drift silently.

import type { LoginPhase } from '../api/types'

/** The rules a code can break, named as the server's refusal detail names them. */
export type CodeRule = 'empty' | 'no_separator' | 'half_missing' | 'extra_hash' | 'bad_character'

/**
 * classify.ValidateCodeShape: after trimming surrounding whitespace, exactly
 * one '#' with something on each side, and nothing but printable ASCII
 * (0x21–0x7e) — a control byte typed into a terminal is a command, not part
 * of a code. Null when the code is well-shaped.
 */
export function checkCodeShape(raw: string): CodeRule | null {
  const code = raw.trim()
  if (code === '') return 'empty'
  for (let i = 0; i < code.length; i++) {
    const c = code.charCodeAt(i)
    if (c <= 0x20 || c >= 0x7f) return 'bad_character'
  }
  const hashes = code.split('#').length - 1
  if (hashes === 0) return 'no_separator'
  if (hashes > 1) return 'extra_hash'
  const [left, right] = code.split('#')
  if (left === '' || right === '') return 'half_missing'
  return null
}

/** What the field says about a code that broke a rule. Never quotes the code. */
export function codeRuleSentence(rule: CodeRule): string {
  switch (rule) {
    case 'empty':
      return 'Paste the code Claude showed you.'
    case 'no_separator':
    case 'half_missing':
      return 'That looks like only half the code. Copy the whole value, both sides of the #.'
    case 'extra_hash':
      return 'That has more than one #. Copy just the one code Claude shows.'
    case 'bad_character':
      return 'A code has no spaces or line breaks inside it. Copy it again.'
  }
}

/** The phases in which a login is still running. */
export function loginLive(phase: LoginPhase): boolean {
  return phase === 'starting' || phase === 'awaiting_code' || phase === 'submitting' || phase === 'invalid_code'
}

/** The phases at which a code may be typed. */
export function acceptsCode(phase: LoginPhase): boolean {
  return phase === 'awaiting_code' || phase === 'invalid_code'
}

/** "4:05" until the deadline; "0:00" once it has passed. */
export function countdown(deadline: string, now: number = Date.now()): string {
  const left = Math.max(0, Math.floor((Date.parse(deadline) - now) / 1000))
  const m = Math.floor(left / 60)
  const s = left % 60
  return `${m}:${String(s).padStart(2, '0')}`
}
