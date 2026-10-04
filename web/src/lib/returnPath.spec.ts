import { describe, expect, it } from 'vitest'
import { DEFAULT_RETURN, safeReturnPath } from './returnPath'

describe('safeReturnPath', () => {
  it('refuses every way out of the origin, and keeps every path on it', () => {
    const hostile: unknown[] = [
      '//evil.example',
      '//evil.example/settings',
      '/\\evil.example',
      '\\\\evil.example',
      '/\t/evil.example',
      '/\n/evil.example',
      '/ /evil.example',
      'https://evil.example/',
      'http://drydock.test/settings', // absolute, even to "us": not a path
      'javascript:alert(1)',
      'data:text/html,<script>alert(1)</script>',
      'evil.example',
      'settings',
      '',
      undefined,
      null,
      ['/settings', '//evil.example'], // ?return=a&return=b
      '/signin',
      '/signin?return=/settings',
    ]
    for (const raw of hostile) {
      expect(safeReturnPath(raw), JSON.stringify(raw)).toBe(DEFAULT_RETURN)
    }

    // Positive controls in the same test: the validator is not simply
    // returning `/` for everything.
    const fine: Array<[string, string]> = [
      ['/settings', '/settings'],
      ['/secrets?filter=db', '/secrets?filter=db'],
      ['/ws/01JABCDEF/logs#latest', '/ws/01JABCDEF/logs#latest'],
      ['/ws/%2F%2Fevil.example', '/ws/%2F%2Fevil.example'], // encoded: a path segment, not a host
      ['/', '/'],
    ]
    for (const [raw, want] of fine) {
      expect(safeReturnPath(raw), raw).toBe(want)
    }
  })
})
