// `drydock.url`, read (vscode-extension §6.4). It is the UI origin and
// nothing else: `https://`, a lowercase host, an optional port, no path, no
// query, no user. The server compares a mutation's `Origin` with its own
// configured origin byte for byte and refuses a capitalised one at startup
// (overall §13.5), so a capital letter here would make every mutation
// `forbidden_origin`; it is refused rather than folded. Parsed by pattern,
// not by URL (which the pure layer does not have, and which would fold the
// case this must refuse).

export type OriginResult = { ok: true; origin: string } | { ok: false; sentence: string }

const ORIGIN = /^https:\/\/([a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*)(?::([0-9]{1,5}))?\/?$/

export const NOT_SET = 'Drydock is not set up in this VS Code yet: set drydock.url to the address of its web UI.'

/** `drydock.url` as an origin, or the sentence saying why it is not one. */
export function originOf(raw: string): OriginResult {
  const s = raw.trim()
  if (s === '') return { ok: false, sentence: NOT_SET }
  const m = ORIGIN.exec(s)
  if (m === null) {
    if (/^https:\/\//i.test(s) && ORIGIN.test(s.toLowerCase())) {
      return { ok: false, sentence: `drydock.url must be written in lowercase, as Drydock's own --ui-origin is: ${s.toLowerCase()}` }
    }
    return {
      ok: false,
      sentence: 'drydock.url must be the address of Drydock\'s web UI, https:// and a host with nothing after it, such as https://drydock.example.com.',
    }
  }
  const host = m[1]!
  const port = m[2]
  if (port !== undefined) {
    const n = Number(port)
    if (n < 1 || n > 65535 || port.startsWith('0')) {
      return { ok: false, sentence: `drydock.url has a port that cannot be: ${port}` }
    }
    // As URL.origin writes it: https's default port is not spelled.
    if (n !== 443) return { ok: true, origin: `https://${host}:${n}` }
  }
  return { ok: true, origin: `https://${host}` }
}
