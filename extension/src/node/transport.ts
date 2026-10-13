// The Transport over Node's fetch, the one piece of the client that touches
// the network. It is not the adapter (it imports nothing from vscode), so the
// client tier runs it against real Caddy with no editor.
//
//   - The URL is sent only as the client spelled it: if parsing it would
//     change it — a dot segment resolved (`%2e%2e` is decoded to `..` and
//     then resolved), a case or an encoding normalised — the request is
//     refused before any connection, so the path the client matched against
//     its routes (routes.ts) is the path the server sees.
//   - `redirect: 'manual'`: a 3xx comes back as itself and is never
//     followed, so the Authorization header cannot reach a Location
//     (transport.spec.ts pins it against a real server).
//   - TLS validation is Node's, fully on. A test CA reaches it only through
//     NODE_EXTRA_CA_CERTS; nothing here (or anywhere in src/, which
//     boundary.spec.ts checks) turns validation off.
//   - Requests time out after REQUEST_TIMEOUT_MS; a stream does not (the
//     stream's own quiet timer ends a silent one).

import type { HttpRequest, HttpResponse, StreamResponse, Transport } from '../ports'

export const REQUEST_TIMEOUT_MS = 30_000

/** A URL the transport will not send, because parsing would change it. */
export class RefusedURL extends Error {
  constructor(what: string) {
    super(`refusing a URL that parsing would change: ${what}`)
    this.name = 'RefusedURL'
  }
}

/** `url`, or a RefusedURL when it is not already in the form fetch would send. */
export function exactURL(url: string): string {
  let parsed: URL
  try {
    parsed = new URL(url)
  } catch {
    throw new RefusedURL('not a URL')
  }
  if (parsed.href !== url || parsed.username !== '' || parsed.password !== '' || parsed.hash !== '') {
    // The path only: the URL as given may be what is wrong, and it is never
    // logged whole.
    throw new RefusedURL(parsed.pathname)
  }
  return url
}

function init(req: HttpRequest, signal: AbortSignal): RequestInit {
  return { method: req.method, headers: req.headers, body: req.body, redirect: 'manual', cache: 'no-store', signal }
}

export const nodeTransport: Transport = {
  async request(req: HttpRequest): Promise<HttpResponse> {
    const resp = await fetch(exactURL(req.url), init(req, AbortSignal.timeout(REQUEST_TIMEOUT_MS)))
    return { status: resp.status, headers: resp.headers, text: () => resp.text() }
  },

  async stream(req: HttpRequest): Promise<StreamResponse> {
    const url = exactURL(req.url)
    const ac = new AbortController()
    const resp = await fetch(url, init(req, ac.signal))
    const body = resp.body
    let cancelled = false
    const cancel = () => {
      if (cancelled) return
      cancelled = true
      ac.abort()
    }
    async function* chunks(): AsyncGenerator<string> {
      if (body === null) return
      const decoder = new TextDecoder()
      const reader = body.getReader()
      try {
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          const text = decoder.decode(value, { stream: true })
          if (text !== '') yield text
        }
        const tail = decoder.decode()
        if (tail !== '') yield tail
      } finally {
        reader.releaseLock()
        cancel()
      }
    }
    return { status: resp.status, headers: resp.headers, chunks: chunks(), cancel }
  },
}
