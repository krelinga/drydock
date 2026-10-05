import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { ApiError } from './client'
import { describeError, inDuration, sentenceFor } from './messages'

describe('describeError', () => {
  it('maps by code and never shows the server prose', () => {
    const prose = 'XYZZY server prose that must never reach the screen'
    const bad = describeError(new ApiError(401, 'bad_password', null, prose))
    expect(bad).toBe('That password is not right.')
    expect(bad).not.toContain('XYZZY')

    // Control: the same prose under a different code gives a different
    // sentence — the code is what decides, not something constant.
    const nc = describeError(new ApiError(503, 'not_configured', null, prose))
    expect(nc).toContain('drydock passwd')
    expect(nc).not.toBe(bad)
    expect(nc).not.toContain('XYZZY')
  })

  it('shows the detail only for the codes whose sentence needs it', () => {
    const detail = 'Drydock refuses it because it disables Remote Control (design §2.1).'
    expect(describeError(new ApiError(400, 'secret_name_reserved', null, 'GH_TOKEN is reserved.', detail)))
      .toBe(`That name is reserved. ${detail}`)
    // Control: a code outside the list never shows a detail, whatever it says.
    const other = describeError(new ApiError(400, 'secret_name_invalid', null, 'prose', 'XYZZY detail'))
    expect(other).toContain("A secret's name is its environment variable name")
    expect(other).not.toContain('XYZZY')
    // And the server's message is still never shown, even beside a detail.
    expect(describeError(new ApiError(400, 'secret_name_reserved', null, 'XYZZY message', detail))).not.toContain('XYZZY')
  })

  it('says when a lockout ends, from Retry-After', () => {
    expect(describeError(new ApiError(429, 'locked_out', 90))).toBe('Too many failed sign-ins. Try again in 2 minutes.')
    expect(describeError(new ApiError(429, 'locked_out', 30))).toBe('Too many failed sign-ins. Try again in 30 seconds.')
    // Without the header it still says lockout, and invents no number.
    const none = describeError(new ApiError(429, 'locked_out', null))
    expect(none).toMatch(/^Too many failed sign-ins\./)
    expect(none).not.toMatch(/\d/)
  })

  it('names an unknown code rather than guessing', () => {
    const msg = describeError(new ApiError(418, 'teapot_overflow', null, 'prose'))
    expect(msg).toContain('teapot_overflow')
    expect(msg).toContain('418')
    expect(msg).not.toContain('prose')
    // Control: a known code does not get the generic treatment.
    expect(describeError(new ApiError(0, 'network'))).not.toContain('Something went wrong')
  })

  it('at_capacity says where the slots are and that Stop frees one, losing nothing; it is not in_progress', () => {
    const cap = describeError(new ApiError(409, 'at_capacity', null, 'prose'))
    expect(cap).toBe('Drydock is at its cap: as many workspaces as it allows are already building or running. '
      + 'Stop one under Running to make room — its clone survives, and Start brings it back.')
    expect(cap).not.toContain('prose')
    // Control: the other 409 says something else entirely.
    expect(describeError(new ApiError(409, 'in_progress'))).toBe('Already in progress.')
  })

  it('has a sentence for every code internal/secrets can refuse with, and the split codes say different things', () => {
    const go = readFileSync(resolve(process.cwd(), '../internal/secrets/validate.go'), 'utf8')
    const codes = [...go.matchAll(/^\s*Code\w+\s+= "([a-z_]+)"$/gm)].map((m) => m[1]!)
    // Control: the parse found the block, new codes included.
    expect(codes).toEqual(expect.arrayContaining(['secret_value_required', 'secret_reach_too_long', 'secret_description_too_long']))
    for (const c of codes) expect(sentenceFor(c), c).toBeDefined()

    // §4.5 #13: absent on a new name and empty are different refusals.
    expect(sentenceFor('secret_value_required')).toBe('A new secret needs a value. There is no stored value to keep.')
    expect(sentenceFor('secret_value_empty')).toContain('cannot be told apart from an unset variable')
    // §4.5 #14: blank and too long are two sentences, neither describing the other.
    expect(sentenceFor('secret_reach_required')).toBe('Say what someone could do with this secret. The answer is required.')
    expect(sentenceFor('secret_reach_too_long')).toBe('The answer to "what can someone do with this?" is too long: at most 2000 bytes.')
    expect(sentenceFor('secret_reach_required')).not.toMatch(/2000|long/)
    expect(sentenceFor('secret_description_too_long')).toBe('The description is too long: at most 4000 bytes.')
    expect(sentenceFor('secret_description_invalid')).toBe('The description must be text.')
  })

  it('confirm_mismatch says nothing was deleted and asks for the name exactly', () => {
    const msg = describeError(new ApiError(400, 'confirm_mismatch', null, 'XYZZY prose'))
    expect(msg).toContain('nothing was deleted')
    expect(msg).toContain('exactly')
    expect(msg).not.toContain('XYZZY')
    // Control: a plain bad_request is not mistaken for it.
    expect(describeError(new ApiError(400, 'bad_request'))).not.toContain('deleted')
  })

  it('handles something that is not an ApiError', () => {
    expect(describeError(new TypeError('x is undefined'))).not.toContain('x is undefined')
    expect(describeError(new ApiError(500, 'internal'))).toContain('internal error')
  })
})

describe('inDuration', () => {
  it('rounds up and never says zero', () => {
    expect(inDuration(0)).toBe('in 1 second')
    expect(inDuration(59)).toBe('in 59 seconds')
    expect(inDuration(60)).toBe('in 1 minute')
    expect(inDuration(61)).toBe('in 2 minutes')
  })
})
