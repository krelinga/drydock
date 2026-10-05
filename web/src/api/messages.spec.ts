import { describe, expect, it } from 'vitest'
import { ApiError } from './client'
import { describeError, inDuration } from './messages'

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

  it('at_capacity points at what to stop, and is not in_progress', () => {
    const cap = describeError(new ApiError(409, 'at_capacity', null, 'prose'))
    expect(cap).toContain('Running section')
    // Control: the other 409 says something else entirely.
    expect(describeError(new ApiError(409, 'in_progress'))).toBe('Already in progress.')
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
