// The Transport over Node's fetch, the one piece of the client that touches
// the network. It is not the adapter (it imports nothing from vscode), so the
// client tier runs it against real Caddy with no editor.
//
//   - `redirect: 'manual'`: a 3xx comes back as itself and is never
//     followed, so the Authorization header cannot reach a Location.
//   - TLS validation is Node's, fully on. A test CA reaches it only through
//     NODE_EXTRA_CA_CERTS; nothing here (or anywhere in src/, which
//     boundary.spec.ts checks) turns validation off.
//   - Requests time out after REQUEST_TIMEOUT_MS; a stream does not (the
//     stream's own quiet timer ends a silent one).

import type { HttpRequest, HttpResponse, StreamResponse, Transport } from '../ports'

export const REQUEST_TIMEOUT_MS = 30_000

function init(req: HttpRequest, signal: AbortSignal): RequestInit {
  return { method: req.method, headers: req.headers, body: req.body, redirect: 'manual', cache: 'no-store', signal }
}

export const nodeTransport: Transport = {
  async request(req: HttpRequest): Promise<HttpResponse> {
    const resp = await fetch(req.url, init(req, AbortSignal.timeout(REQUEST_TIMEOUT_MS)))
    return { status: resp.status, headers: resp.headers, text: () => resp.text() }
  },

  async stream(req: HttpRequest): Promise<StreamResponse> {
    const ac = new AbortController()
    const resp = await fetch(req.url, init(req, ac.signal))
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
