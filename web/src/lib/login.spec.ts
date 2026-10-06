// The client's copy of the code-shape rule (design §7.2, frontend §6.2),
// against the server's own cases and its source.

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { acceptsCode, checkCodeShape, codeRuleSentence, countdown, loginLive, type CodeRule } from './login'

const CLASSIFY = readFileSync(resolve(process.cwd(), '../internal/classify/login.go'), 'utf8')
const ROUTES = readFileSync(resolve(process.cwd(), '../internal/api/claude_routes.go'), 'utf8')

// internal/classify/login_test.go's TestValidateCodeShape, case for case.
const L = 'Kq7vZ2mXwP9rT4bNe8Jd'
const R = 'Hs3jY8cLdF6gA1eUo5Wy'

describe('checkCodeShape', () => {
  it.each([L + '#' + R, L + '#' + R + '\n', L + '#' + R + '\r\n', '  ' + L + '#' + R + '\t', 'a#b'])('accepts %j', (c) => {
    expect(checkCodeShape(c)).toBeNull()
  })

  it.each<[string, string, CodeRule]>([
    ['empty', '', 'empty'],
    ['blank', ' \n', 'empty'],
    ['no hash', L + R, 'no_separator'],
    ['left half missing', '#' + R, 'half_missing'],
    ['right half missing', L + '#', 'half_missing'],
    ['right half missing, newline', L + '#\n', 'half_missing'],
    ['two hashes', L + '#' + R + '#' + L, 'extra_hash'],
    ['trailing hash', L + '#' + R + '#', 'extra_hash'],
    ['space inside left', L.slice(0, 8) + ' ' + L.slice(8) + '#' + R, 'bad_character'],
    ['tab inside right', L + '#' + R.slice(0, 8) + '\t' + R.slice(8), 'bad_character'],
    ['newline inside', L + '\n#' + R, 'bad_character'],
    ['ctrl-c', L + '\x03#' + R, 'bad_character'],
    ['escape', L + '#' + R + '\x1b[A', 'bad_character'],
    ['del', L + '#\x7f' + R, 'bad_character'],
    ['non-ascii', L + 'é#' + R, 'bad_character'],
  ])('%s', (_, code, rule) => {
    expect(checkCodeShape(code)).toBe(rule)
    // No sentence quotes the code or either half.
    const s = codeRuleSentence(rule)
    expect(s).not.toContain(L.slice(0, 6))
    expect(s).not.toContain(R.slice(0, 6))
  })

  it('is the server rule: the same five names, the same printable-ASCII bound', () => {
    // The route's refusal detail names exactly these rules (claude_routes.go codeRule).
    const named = [...ROUTES.matchAll(/return "([a-z_]+)"\n/g)].map((m) => m[1]).filter((n) => n !== '')
    expect(new Set(named)).toEqual(new Set<CodeRule>(['empty', 'no_separator', 'half_missing', 'extra_hash', 'bad_character']))
    // classify.ValidateCodeShape's bound, which checkCodeShape copies.
    expect(CLASSIFY).toContain('if c := code[i]; c <= 0x20 || c >= 0x7f {')
  })
})

describe('phases and the countdown', () => {
  it('a wrong code is a loop: invalid_code still takes a code and is still live', () => {
    expect(acceptsCode('invalid_code')).toBe(true)
    expect(loginLive('invalid_code')).toBe(true)
    expect(acceptsCode('submitting')).toBe(false)
    expect(acceptsCode('starting')).toBe(false)
    for (const p of ['succeeded', 'timed_out', 'failed', 'cancelled'] as const) {
      expect(loginLive(p)).toBe(false)
      expect(acceptsCode(p)).toBe(false)
    }
  })

  it('counts down to the deadline and stops at zero', () => {
    const now = Date.parse('2026-10-06T12:00:00Z')
    expect(countdown('2026-10-06T12:05:00Z', now)).toBe('5:00')
    expect(countdown('2026-10-06T12:00:09Z', now)).toBe('0:09')
    expect(countdown('2026-10-06T11:59:00Z', now)).toBe('0:00')
  })
})
