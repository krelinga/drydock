// A tap between Caddy and a Drydock socket, shared by the browser tier and
// the extension's client tier. It forwards bytes unchanged and records what
// arrived — whether the session cookie came, which Authorization header,
// which Origin — the only place "the client did not send it" can be asserted
// at the server. It streams; it does not buffer, so an SSE assertion still
// measures Caddy. Upgrades are relayed byte for byte.

import http from 'node:http'
import net from 'node:net'

/** The session cookie (internal/auth), whose arrival every record notes. */
export const COOKIE = '__Host-drydock'

/** What the tap saw for one request. */
export interface Seen {
  seq: number
  socket: 'api' | 'preview'
  method: string
  path: string
  host: string
  origin: string | null
  referer: string | null
  /** The value of the session cookie, if one arrived. */
  session: string | null
  cookieNames: string[]
  secFetchSite: string | null
  secFetchDest: string | null
  secFetchMode: string | null
  lastEventId: string | null
  acceptEncoding: string | null
  /** The Authorization header as it arrived (a bearer token, for the extension's tier). */
  authorization: string | null
  status: number | null
  responseHeaders: http.IncomingHttpHeaders | null
  /** The Upgrade header of a request relayed as an upgrade, else absent. */
  upgrade?: string | null
}

export class Tap {
  readonly seen: Seen[] = []
  private seq = 0
  private server: http.Server

  constructor(
    readonly socket: 'api' | 'preview',
    listen: string | { host: string; port: number },
    upstream: string,
    /** Rewrites forwarded headers, after the record is taken (the loopback front). */
    rewrite?: { request?: (h: http.IncomingHttpHeaders) => void; response?: (h: http.IncomingHttpHeaders) => void },
  ) {
    this.server = http.createServer((req, res) => {
      const cookies = parseCookies(req.headers.cookie)
      const rec: Seen = {
        seq: ++this.seq,
        socket,
        method: req.method ?? '',
        path: req.url ?? '',
        host: req.headers.host ?? '',
        origin: header(req, 'origin'),
        referer: header(req, 'referer'),
        session: cookies.get(COOKIE) ?? null,
        cookieNames: [...cookies.keys()],
        secFetchSite: header(req, 'sec-fetch-site'),
        secFetchDest: header(req, 'sec-fetch-dest'),
        secFetchMode: header(req, 'sec-fetch-mode'),
        lastEventId: header(req, 'last-event-id'),
        acceptEncoding: header(req, 'accept-encoding'),
        authorization: header(req, 'authorization'),
        status: null,
        responseHeaders: null,
      }
      this.seen.push(rec)
      const headers = { ...req.headers }
      rewrite?.request?.(headers)
      const up = http.request(
        { socketPath: upstream, method: req.method, path: req.url, headers },
        (ur) => {
          rec.status = ur.statusCode ?? null
          rec.responseHeaders = ur.headers
          const out = { ...ur.headers }
          rewrite?.response?.(out)
          res.writeHead(ur.statusCode ?? 502, out)
          // Streams each chunk as it arrives: SSE frames are not held here.
          ur.on('data', (c: Buffer) => res.write(c))
          ur.on('end', () => res.end())
          ur.on('error', () => res.destroy())
        },
      )
      up.on('error', () => {
        if (!res.headersSent) res.writeHead(502)
        res.end()
      })
      res.on('close', () => up.destroy())
      req.pipe(up)
    })
    // An upgrade (a preview's HMR websocket) is relayed byte for byte, and
    // recorded like any request: the 101's status and headers are read off
    // the first bytes back.
    this.server.on('upgrade', (req: http.IncomingMessage, socket: net.Socket, head: Buffer) => {
      const cookies = parseCookies(req.headers.cookie)
      const rec: Seen = {
        seq: ++this.seq,
        socket: this.socket,
        method: req.method ?? '',
        path: req.url ?? '',
        host: req.headers.host ?? '',
        origin: header(req, 'origin'),
        referer: header(req, 'referer'),
        session: cookies.get(COOKIE) ?? null,
        cookieNames: [...cookies.keys()],
        secFetchSite: header(req, 'sec-fetch-site'),
        secFetchDest: header(req, 'sec-fetch-dest'),
        secFetchMode: header(req, 'sec-fetch-mode'),
        lastEventId: header(req, 'last-event-id'),
        acceptEncoding: header(req, 'accept-encoding'),
        authorization: header(req, 'authorization'),
        status: null,
        responseHeaders: null,
        upgrade: header(req, 'upgrade'),
      }
      this.seen.push(rec)
      const up = net.connect(upstream)
      const close = () => {
        up.destroy()
        socket.destroy()
      }
      up.on('error', close)
      socket.on('error', close)
      up.on('connect', () => {
        let lines = `${req.method} ${req.url} HTTP/${req.httpVersion}\r\n`
        for (let i = 0; i + 1 < req.rawHeaders.length; i += 2) lines += `${req.rawHeaders[i]}: ${req.rawHeaders[i + 1]}\r\n`
        up.write(lines + '\r\n')
        if (head.length) up.write(head)
        let first = true
        up.on('data', (c: Buffer) => {
          if (first) {
            first = false
            const text = c.toString('latin1')
            const end = text.indexOf('\r\n\r\n')
            const [status, ...hs] = text.slice(0, end < 0 ? undefined : end).split('\r\n')
            rec.status = Number((status ?? '').split(' ')[1]) || null
            const h: http.IncomingHttpHeaders = {}
            for (const l of hs) {
              const i = l.indexOf(':')
              if (i < 0) continue
              const k = l.slice(0, i).trim().toLowerCase()
              const v = l.slice(i + 1).trim()
              if (k === 'set-cookie') h['set-cookie'] = [...(h['set-cookie'] ?? []), v]
              else h[k] = v
            }
            rec.responseHeaders = h
          }
          socket.write(c)
        })
        socket.on('data', (c: Buffer) => up.write(c))
        up.on('end', () => socket.end())
        socket.on('end', () => up.end())
        up.on('close', close)
        socket.on('close', close)
      })
    })
    this.listening = new Promise((r) => this.server.once('listening', r))
    if (typeof listen === 'string') this.server.listen(listen)
    else this.server.listen(listen.port, listen.host)
  }

  readonly listening: Promise<unknown>

  /** The TCP port, for a tap listening on one. */
  port(): number {
    const a = this.server.address()
    if (typeof a !== 'object' || !a) throw new Error('tap: not listening on TCP')
    return a.port
  }

  clear(): void {
    this.seen.length = 0
  }

  close(): Promise<void> {
    this.server.closeAllConnections()
    return new Promise((r) => this.server.close(() => r()))
  }
}

export function header(req: http.IncomingMessage, name: string): string | null {
  const v = req.headers[name]
  return typeof v === 'string' ? v : Array.isArray(v) ? v.join(', ') : null
}

export function parseCookies(raw: string | undefined): Map<string, string> {
  const out = new Map<string, string>()
  for (const part of (raw ?? '').split(';')) {
    const i = part.indexOf('=')
    if (i > 0) out.set(part.slice(0, i).trim(), part.slice(i + 1).trim())
  }
  return out
}
