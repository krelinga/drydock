// The `return` parameter on /signin, which the app navigates to after a
// successful sign-in (frontend §4.4).
//
// Anything that reaches /signin can set it, so it is an open redirect unless it
// is held to "a path on this origin". The rule is deliberately narrower than
// "parses to this origin": accept only a string that starts with exactly one
// `/`, contains no backslash and no control or whitespace character, and
// resolves to the same origin. Each exclusion is a real bypass:
//
//   //evil.example        protocol-relative: a different host
//   /\evil.example        browsers read `\` as `/`, so this is `//evil.example`
//   /<TAB>/evil.example   the URL parser strips tab and newline, same result
//   https://evil.example  absolute
//   javascript:…          not a path at all
//
// A rejected value falls back to `/` rather than being repaired; there is no
// safe way to guess what a hostile string meant.

export const DEFAULT_RETURN = '/'

export function safeReturnPath(raw: unknown): string {
  if (typeof raw !== 'string' || raw === '') return DEFAULT_RETURN
  if (raw[0] !== '/' || raw[1] === '/' || raw[1] === '\\') return DEFAULT_RETURN
  if (/[\\\u0000-\u0020\u007f]/.test(raw)) return DEFAULT_RETURN

  const base = 'https://return.invalid'
  let url: URL
  try {
    url = new URL(raw, base)
  } catch {
    return DEFAULT_RETURN
  }
  if (url.origin !== base) return DEFAULT_RETURN
  // Returning to the sign-in page after signing in is a loop, not a place.
  if (url.pathname === '/signin') return DEFAULT_RETURN
  return url.pathname + url.search + url.hash
}
