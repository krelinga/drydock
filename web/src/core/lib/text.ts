// Showing a string some repository wrote (vscode-extension §10.4): a
// setting's field, value and source, a branch name, a repository's full name.
// Each is shown as data and never interpreted, and nothing in it may change
// how the text around it reads.
//
// Three kinds of code point can: control characters (a newline forges a line
// in a text document, U+0085 too), the line and paragraph separators (U+2028,
// U+2029, which some renderers break on), and the format characters — bidi
// overrides and isolates (U+202A–U+202E, U+2066–U+2069) that reorder what
// follows them on screen, and the zero-width characters (U+200B–U+200D,
// U+FEFF) that hide a difference between two strings that look the same.
// Each is replaced by its escape, `\uXXXX` (or `\u{XXXXX}` past the BMP), so
// it shows as six visible characters where it was. Everything else, accented
// letters and emoji included, is left alone.

const INVISIBLE = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu

/** `s` with every control, format and separator character shown as its `\u` escape. */
export function escapeFormat(s: string): string {
  return s.replace(INVISIBLE, (ch) => {
    const cp = ch.codePointAt(0)!
    const hex = cp.toString(16).toUpperCase()
    return cp > 0xffff ? `\\u{${hex}}` : `\\u${hex.padStart(4, '0')}`
  })
}

/**
 * A value as JSON text, escaped: `JSON.stringify` already escapes C0
 * controls inside strings, and `escapeFormat` takes the rest. A value the
 * server left out (`undefined`) is shown as nothing, as before.
 */
export function showValue(v: unknown): string {
  const json = JSON.stringify(v)
  return json === undefined ? '' : escapeFormat(json)
}
