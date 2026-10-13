// The one HTTP client (vscode-extension §4.2, §4.4, §8 rule 5). Every
// request the extension makes goes through here, which is what makes these
// hold everywhere rather than wherever someone remembered them:
//
//  1. The token goes only to the origin it was stored for. The URL is the
//     configured origin and a path; a credential stored for any other origin
//     sends nothing at all and asks for a sign-in (`SignInNeeded`).
//  2. Only a contracted path (routes.ts) is sent, with its method.
//  3. A redirect is never followed: the transport returns a 3xx as itself,
//     and here it is an error. A token never travels to a Location.
//  4. Every mutation carries `Origin: <the configured origin>`, byte for
//     byte (§4.4): the server's exact-match check is kept for every
//     credential, and a token is not ambient, so this forges nothing.
//  5. A 401 is the one 401 path: the stored credential is cleared (the
//     token was revoked or expired) before the error is thrown. Nothing else
//     clears it — a `403 forbidden_scope` leaves it, and a 503 is the
//     server's trouble, not the token's.
//  6. A mutation's body is discarded (frontend §4.2): entities come from the
//     stream's reducer alone.
//
// Errors are core's ApiError, so core/messages.ts gives the extension the
// web UI's sentence for each code.

import { ApiError } from '../../web/src/core/errors'
import type { CredentialStore } from './credential'
import type { HttpRequest, HttpResponse, StreamResponse, Transport } from './ports'
import { routeOf } from './routes'
import { NOT_SET } from './settings'

type Method = HttpRequest['method']

/** No credential for the configured origin: sign in (again). */
export class SignInNeeded extends Error {
  constructor(readonly reason: 'none' | 'other_origin') {
    super(reason === 'none' ? 'Sign in to Drydock first.' : 'drydock.url changed since you signed in, so sign in again.')
    this.name = 'SignInNeeded'
  }
}

/** `drydock.url` is unset or not an origin. */
export class NotConfigured extends Error {
  constructor(readonly sentence: string = NOT_SET) {
    super(sentence)
    this.name = 'NotConfigured'
  }
}

export interface ClientDeps {
  transport: Transport
  credentials: CredentialStore
  /** The configured origin, or the sentence saying why there is none. */
  origin: () => { ok: true; origin: string } | { ok: false; sentence: string }
  /** The one 401 path, after the credential is cleared. */
  onUnauthorized?: () => void
}

export class DrydockClient {
  constructor(private readonly deps: ClientDeps) {}

  /** A read; the body is the point, so it is parsed. */
  async get<T>(path: string): Promise<T> {
    const resp = await this.request('GET', path)
    return (await json(resp)) as T
  }

  /** A mutation: resolves when the server accepted it (2xx), returning nothing. */
  async send(method: Exclude<Method, 'GET'>, path: string, body?: unknown): Promise<void> {
    const resp = await this.request(method, path, body)
    // Drained so the connection is reused, and never parsed.
    await resp.text().catch(() => '')
  }

  /**
   * GET /api/meta with a token not yet stored: how a pasted token is verified
   * (§4.2). A 401 here clears nothing (nothing is stored for it) and is the
   * caller's to explain.
   */
  async metaWith(origin: string, token: string): Promise<unknown> {
    const resp = await this.raw({ method: 'GET', url: origin + '/api/meta', headers: this.headers('GET', origin, token) }, false)
    return json(resp)
  }

  /** Opens the event stream, resuming after `lastEventId` when it is past 0. */
  async stream(lastEventId: number): Promise<StreamResponse> {
    const { origin, token } = await this.target('GET', '/api/events')
    const headers: Record<string, string> = { ...this.headers('GET', origin, token), Accept: 'text/event-stream' }
    if (lastEventId > 0) headers['Last-Event-ID'] = String(lastEventId)
    const resp = await this.deps.transport.stream({ method: 'GET', url: origin + '/api/events', headers })
    if (resp.status === 200) return resp
    resp.cancel()
    throw await this.failure({ status: resp.status, headers: resp.headers, text: async () => '' }, true)
  }

  private async request(method: Method, path: string, body?: unknown): Promise<HttpResponse> {
    const { origin, token } = await this.target(method, path)
    const req: HttpRequest = { method, url: origin + path, headers: this.headers(method, origin, token) }
    if (body !== undefined) {
      req.headers['Content-Type'] = 'application/json'
      req.body = JSON.stringify(body)
    }
    return this.raw(req, true)
  }

  private async target(method: Method, path: string): Promise<{ origin: string; token: string }> {
    if (routeOf(method, path) === null) throw new Error(`drydock client: refusing ${method} ${path}, which is no route the extension uses`)
    const o = this.deps.origin()
    if (!o.ok) throw new NotConfigured(o.sentence)
    const stored = await this.deps.credentials.read()
    if (stored === null) throw new SignInNeeded('none')
    if (stored.origin !== o.origin) throw new SignInNeeded('other_origin')
    return { origin: o.origin, token: stored.token }
  }

  private headers(method: Method, origin: string, token: string): Record<string, string> {
    const h: Record<string, string> = { Accept: 'application/json', Authorization: `Bearer ${token}` }
    if (method !== 'GET') h.Origin = origin
    return h
  }

  private async raw(req: HttpRequest, stored: boolean): Promise<HttpResponse> {
    let resp: HttpResponse
    try {
      resp = await this.deps.transport.request(req)
    } catch {
      throw new ApiError(0, 'network')
    }
    if (resp.status >= 200 && resp.status < 300) return resp
    throw await this.failure(resp, stored)
  }

  private async failure(resp: HttpResponse, stored: boolean): Promise<ApiError> {
    if (resp.status >= 300 && resp.status < 400) return new ApiError(resp.status, 'redirect')
    const err = await toError(resp)
    if (resp.status === 401 && stored) {
      await this.deps.credentials.clear()
      this.deps.onUnauthorized?.()
    }
    return err
  }
}

async function json(resp: HttpResponse): Promise<unknown> {
  if (!(resp.headers.get('Content-Type') ?? '').startsWith('application/json')) throw new ApiError(resp.status, 'unparseable')
  try {
    return JSON.parse(await resp.text())
  } catch {
    throw new ApiError(resp.status, 'unparseable')
  }
}

async function toError(resp: HttpResponse): Promise<ApiError> {
  const ra = resp.headers.get('Retry-After')
  const retryAfter = ra !== null && /^\d+$/.test(ra.trim()) ? Number(ra.trim()) : null
  if (!(resp.headers.get('Content-Type') ?? '').startsWith('application/json')) {
    return new ApiError(resp.status, 'unparseable', retryAfter)
  }
  try {
    const env = JSON.parse(await resp.text()) as { error?: { code?: unknown; message?: unknown; detail?: unknown } }
    const code = env.error?.code
    if (typeof code !== 'string' || code === '') return new ApiError(resp.status, 'unparseable', retryAfter)
    const message = typeof env.error?.message === 'string' ? env.error.message : ''
    const detail = typeof env.error?.detail === 'string' ? env.error.detail : ''
    return new ApiError(resp.status, code, retryAfter, message, detail)
  } catch {
    return new ApiError(resp.status, 'unparseable', retryAfter)
  }
}
