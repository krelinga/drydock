// The one fetch wrapper. Every request the app makes goes through here
// (frontend §3, §8), which is what makes three properties hold everywhere
// rather than wherever someone remembered them:
//
//  1. Any 401 from any route is "you are signed out now" (§2.2, §4.4). The
//     cookie is HttpOnly, so traffic is the only way the app can know.
//  2. A mutation's response body is discarded (§4.2 step 3). Entity state is
//     written by the stream's reducer and nothing else; `send` returns void so
//     there is nothing to be tempted by.
//  3. Requests are same-origin JSON fetches: `credentials: 'same-origin'`,
//     never `mode: 'no-cors'`, never a native form submission. That is what
//     makes the browser send the `Origin` header the server's exact-match
//     check needs (§13.3).
//
// Errors are reported by the envelope's machine-readable `code`; the server's
// prose `message` is kept for logging but never shown (see messages.ts).

import type { ErrorCode, ErrorEnvelope } from './types'

export class ApiError extends Error {
  constructor(
    /** HTTP status, or 0 when no response arrived. */
    readonly status: number,
    /** The envelope's code, or `network` / `unparseable`. */
    readonly code: ErrorCode | (string & {}),
    /** Seconds from a `Retry-After` header, when one was sent as seconds. */
    readonly retryAfter: number | null = null,
    serverMessage = '',
  ) {
    super(serverMessage || `${status} ${code}`)
    this.name = 'ApiError'
  }
}

type UnauthorizedHandler = () => void
let unauthorized: UnauthorizedHandler | null = null

/**
 * Registers what a 401 does. The app installs exactly one (auth.ts); it is a
 * setter rather than an import so this module stays free of the router and the
 * stores, and so a test can observe it.
 */
export function onUnauthorized(fn: UnauthorizedHandler | null): void {
  unauthorized = fn
}

type Method = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'

async function request(method: Method, path: string, body?: unknown): Promise<Response> {
  if (!path.startsWith('/api/')) {
    // A guard against building a URL from data: the client talks to its own
    // API and nothing else.
    throw new Error(`api client: refusing non-API path ${path}`)
  }
  const headers: Record<string, string> = { Accept: 'application/json' }
  const init: RequestInit = {
    method,
    headers,
    credentials: 'same-origin',
    mode: 'same-origin',
    cache: 'no-store',
  }
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }

  let resp: Response
  try {
    resp = await fetch(new URL(path, window.location.origin), init)
  } catch {
    throw new ApiError(0, 'network')
  }

  if (resp.ok) return resp

  const err = await toError(resp)
  if (resp.status === 401) {
    // Any 401, from anywhere, including the sign-in POST's bad password: the
    // handler is idempotent when already signed out, and treating every 401
    // alike is what keeps a future route's 401 from being the one that
    // leaves stale data on screen.
    unauthorized?.()
  }
  throw err
}

async function toError(resp: Response): Promise<ApiError> {
  const ra = resp.headers.get('Retry-After')
  const retryAfter = ra !== null && /^\d+$/.test(ra.trim()) ? Number(ra.trim()) : null
  const type = resp.headers.get('Content-Type') ?? ''
  if (!type.startsWith('application/json')) {
    return new ApiError(resp.status, 'unparseable', retryAfter)
  }
  try {
    const env = (await resp.json()) as Partial<ErrorEnvelope>
    const code = env.error?.code
    if (typeof code !== 'string' || code === '') {
      return new ApiError(resp.status, 'unparseable', retryAfter)
    }
    return new ApiError(resp.status, code, retryAfter, env.error?.message ?? '')
  } catch {
    return new ApiError(resp.status, 'unparseable', retryAfter)
  }
}

/** A read. The body is the point, so it is parsed and returned. */
export async function get<T>(path: string): Promise<T> {
  const resp = await request('GET', path)
  const type = resp.headers.get('Content-Type') ?? ''
  if (!type.startsWith('application/json')) {
    throw new ApiError(resp.status, 'unparseable')
  }
  return (await resp.json()) as T
}

/**
 * A mutation. Resolves when the server accepted it (2xx) and returns nothing:
 * §4.2 step 3, the body of a 202 is already stale and applying it would be a
 * second write path for entity state.
 */
export async function send(method: Exclude<Method, 'GET'>, path: string, body?: unknown): Promise<void> {
  const resp = await request(method, path, body)
  // Drain without parsing, so the connection can be reused.
  await resp.body?.cancel()
}
