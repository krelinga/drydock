// The client's copy of design §10.1's rules. Two things are tested: that each
// check says exactly what the server says, and that the copy has not drifted
// from internal/secrets/validate.go — read here as text, so a name the server
// learns to refuse fails this spec until the form refuses it too.

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  MAX_DESCRIPTION_LEN, MAX_NAME_LEN, MAX_REACH_LEN, MAX_VALUE_LEN, RESERVED_EXACT, RESERVED_PREFIX, checkDescription,
  checkName, checkReach, checkValue, reservedReason,
} from './secretRules'

// Vitest runs from web/ (jsdom's import.meta.url is not a file URL).
const GO = readFileSync(resolve(process.cwd(), '../internal/secrets/validate.go'), 'utf8')

/** A Go block between two markers. */
function block(start: string, end = '\n}\n'): string {
  const i = GO.indexOf(start)
  expect(i, `validate.go has ${start}`).toBeGreaterThanOrEqual(0)
  return GO.slice(i, GO.indexOf(end, i))
}

/** Go's interpreted string literal → its value, for the escapes validate.go uses. */
const unquote = (s: string) => JSON.parse(`"${s}"`) as string

describe('the reserved list matches validate.go', () => {
  it('names the same exact names, each with the same reason', () => {
    const go: Record<string, string> = {}
    for (const m of block('var reservedExact').matchAll(/^\s*"([^"]+)":\s*"((?:[^"\\]|\\.)*)",$/gm)) go[m[1]!] = unquote(m[2]!)
    // Control: the parse found the list, so an empty one cannot pass.
    expect(Object.keys(go).length).toBeGreaterThan(25)
    expect(go.GITHUB_TOKEN).toContain('gh shim')
    expect(RESERVED_EXACT).toEqual(go)
  })

  it('names the same prefixes in the same order, each with the same reason', () => {
    const go: Array<[string, string]> = []
    for (const m of block('var reservedPrefix').matchAll(/\{"([A-Z_]+)",\s*"((?:[^"\\]|\\.)*)"\}/g)) go.push([m[1]!, unquote(m[2]!)])
    expect(go.length).toBeGreaterThan(5)
    expect(RESERVED_PREFIX.map(([p, w]) => [p, w])).toEqual(go)
  })

  it('has the same limits', () => {
    const lim = block('const (\n\tMaxNameLen', ')')
    expect(lim).toMatch(new RegExp(`MaxNameLen\\s+= ${MAX_NAME_LEN}\\b`))
    expect(lim).toMatch(/MaxValueLen\s+= 32 << 10/)
    expect(MAX_VALUE_LEN).toBe(32 << 10)
    expect(lim).toMatch(new RegExp(`MaxReachLen\\s+= ${MAX_REACH_LEN}\\b`))
    expect(lim).toMatch(new RegExp(`MaxDescriptionLen\\s+= ${MAX_DESCRIPTION_LEN}\\b`))
  })

  it('and the detail sentences are the server\'s', () => {
    // Each detail the client composes is a format string in validate.go.
    expect(GO).toContain('"Drydock refuses it because " + why + "."')
    expect(GO).toContain('"It contains %s at byte %d. A multi-line credential, such as a PEM, goes in as base64."')
    expect(GO).toContain('"Use capital letters, digits and underscores, not starting with a digit, at most %d characters."')
    expect(GO).toContain('"An empty value cannot be told apart from an unset variable."')
    expect(GO).toContain('"The reach field is required: it is the decision to grant, written down."')
  })
})

describe('checkName', () => {
  it('refuses a reserved name with its reason, and accepts an ordinary one', () => {
    expect(checkName('GH_TOKEN')).toEqual({
      code: 'secret_name_reserved',
      detail: "Drydock refuses it because gh reads GH_* itself; GH_TOKEN would shadow the shim's repository-scoped token (§9.2).",
    })
    expect(checkName('DO_NOT_TRACK')?.detail).toBe('Drydock refuses it because it disables Remote Control (design §2.1).')
    expect(checkName('CLAUDE_ENV_FILE')?.code).toBe('secret_name_reserved')
    // Controls: near misses that are not reserved.
    expect(checkName('STRIPE_TEST_KEY')).toBeNull()
    expect(checkName('GHX_TOKEN')).toBeNull()
    expect(reservedReason('MY_GIT_TOKEN')).toBeNull()
  })

  it('refuses what is not an environment variable name', () => {
    for (const bad of ['', 'lower', '9LIVES', 'A-B', 'A B', 'É', 'A'.repeat(MAX_NAME_LEN + 1)]) {
      expect(checkName(bad)?.code, bad).toBe('secret_name_invalid')
    }
    expect(checkName('A'.repeat(MAX_NAME_LEN))).toBeNull()
    expect(checkName('_X9')).toBeNull()
  })
})

describe('checkValue', () => {
  it('names the character and its UTF-8 byte offset, never the value', () => {
    const r = checkValue('hunter2\nGH_TOKEN ghp_x')
    expect(r).toEqual({
      code: 'secret_value_control_character',
      detail: 'It contains a newline (U+000A) at byte 7. A multi-line credential, such as a PEM, goes in as base64.',
    })
    expect(r!.detail).not.toContain('hunter2')
    // Offsets are bytes, as Go's range over a string reports them: é is two.
    expect(checkValue('é\r')?.detail).toContain('a carriage return (U+000D) at byte 2')
    expect(checkValue('a\u0000')?.detail).toContain('a NUL (U+0000) at byte 1')
    expect(checkValue('\t')?.detail).toContain('a tab (U+0009) at byte 0')
    expect(checkValue('x\u007f')?.detail).toContain('the control character U+007F at byte 1')
    expect(checkValue('x\u0085')?.detail).toContain('the control character U+0085 at byte 1') // C1
    // Controls: printable text, including non-ASCII and a space, passes.
    expect(checkValue('postgres://u:p@h/db?x=é ✓')).toBeNull()
  })

  it('refuses empty and over-long values', () => {
    expect(checkValue('')?.code).toBe('secret_value_empty')
    expect(checkValue('a'.repeat(MAX_VALUE_LEN + 1))?.code).toBe('secret_value_too_long')
    // Bytes, not characters: 16385 two-byte characters is over.
    expect(checkValue('é'.repeat(MAX_VALUE_LEN / 2 + 1))?.code).toBe('secret_value_too_long')
    expect(checkValue('a'.repeat(MAX_VALUE_LEN))).toBeNull()
  })
})

describe('checkReach and checkDescription', () => {
  it('a reach is required and bounded; a description is optional and bounded', () => {
    expect(checkReach('   \n ')?.code).toBe('secret_reach_required')
    expect(checkReach('x'.repeat(MAX_REACH_LEN + 1))?.code).toBe('secret_reach_required')
    expect(checkReach('Read the staging database.')).toBeNull()
    expect(checkDescription('')).toBeNull()
    expect(checkDescription('x'.repeat(MAX_DESCRIPTION_LEN + 1))?.code).toBe('secret_description_invalid')
  })
})
