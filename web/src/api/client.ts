// The one fetch wrapper. Every request the app makes goes through here
// (frontend §3, §8), which is what makes three properties hold everywhere
// rather than wherever someone remembered them:
//
//  1. Any 401 from any route is "you are signed out now" (§2.2, §4.4). The
//     cookie is HttpOnly, so traffic is the only way the app can know.
//  2. A mutation's response body is discarded (§4.2 step 3). Entity state is
//     written by the stream's reducer and nothing else; `send` returns void so
//     there is nothing to be tempted by. The one exception, `sendForResult`,
//     returns an operation's result for its own screen, never for the reducer.
//  3. Requests are same-origin JSON fetches: `credentials: 'same-origin'`,
//     `mode: 'same-origin'`, never `mode: 'no-cors'`, never a native form
//     submission. That is what makes the browser send the `Origin` header the
//     server's exact-match check needs (§13.3).
//  4. Every request carries `referrerPolicy: 'same-origin'`, overriding the
//     document's `no-referrer` for Drydock's own API and nothing else (§8).
//     Without it, the Fetch standard's "append a request Origin header"
//     serializes the Origin of a non-GET request whose mode is not `cors`
//     through the referrer policy, and `no-referrer` makes it `null`: Safari
//     and Firefox send `Origin: null` on every mutation, sign-in included, and
//     the server's exact match refuses it with `forbidden_origin` (v0.2.1).
//     Chromium sends the real origin either way, which is how it shipped.
//     Under `same-origin` the serialization yields the real origin for a
//     same-origin request; the cost is a `Referer` carrying the page's URL,
//     sent to Drydock alone, since `mode: 'same-origin'` refuses any other
//     destination. The document-level policy stays `no-referrer`, so links
//     out (GitHub, claude.ai) still carry no Drydock URL.
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
    /**
     * The envelope's `detail`: which rule, which character. Shown as text for
     * the few codes whose sentence needs it (messages.ts), never parsed.
     */
    readonly detail = '',
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

async function request(method: Method, path: string, body?: unknown, extra: Record<string, string> = {}): Promise<Response> {
  if (!path.startsWith('/api/')) {
    // A guard against building a URL from data: the client talks to its own
    // API and nothing else.
    throw new Error(`api client: refusing non-API path ${path}`)
  }
  const headers: Record<string, string> = { ...extra, Accept: 'application/json' }
  const init: RequestInit = {
    method,
    headers,
    credentials: 'same-origin',
    mode: 'same-origin',
    // Point 4 above: without this, Safari and Firefox send `Origin: null`.
    referrerPolicy: 'same-origin',
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
    const detail = typeof env.error?.detail === 'string' ? env.error.detail : ''
    return new ApiError(resp.status, code, retryAfter, env.error?.message ?? '', detail)
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

/**
 * A mutation whose 2xx body is an operation's *result*, not entity state.
 *
 * One route needs it: `PUT /api/secrets/:name`, whose 200 says which running
 * workspaces a rotation reached (frontend §4.5 #5, §6.4). That list answers
 * "what did my rotation cost?", which belongs to the screen that asked, and
 * the server persists it nowhere. The same body carries the secret's
 * metadata too, and that must not be applied: it has no event id to be
 * ordered by, so the caller strips it (stores/secrets.ts) and the reducer
 * learns the secret from its event like everything else (§2.1, §4.1).
 *
 * `headers` carries the one request header a caller may add: `If-None-Match:
 * *`, which makes that PUT a create the server refuses to turn into a
 * replace (`412 secret_exists`).
 */
export async function sendForResult<T>(
  method: Exclude<Method, 'GET'>, path: string, body?: unknown, headers: Record<string, string> = {},
): Promise<T> {
  const resp = await request(method, path, body, headers)
  const type = resp.headers.get('Content-Type') ?? ''
  if (!type.startsWith('application/json')) {
    throw new ApiError(resp.status, 'unparseable')
  }
  return (await resp.json()) as T
}
