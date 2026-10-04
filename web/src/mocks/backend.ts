// A pretend Drydock for the MSW harness (frontend §10): the three
// /api/auth/session routes and the gate's behaviour for everything else,
// shaped exactly as internal/api writes them — the same codes, the same
// statuses, the same Retry-After, the same 401-before-404 ordering for an
// unknown /api path. The specs and `npm run dev:mock` share it, so what the UI
// is developed against is what it is tested against.
//
// State is in memory and per process. There is no cookie: the browser's
// HttpOnly cookie cannot be faked from a service worker, so "signed in" is a
// flag here. That is the one place this harness is not the server, and it is
// why cookie semantics belong to the browser tier (testing §10).

import { http, HttpResponse, type HttpHandler } from 'msw'
import type { Device, SessionInfo } from '../api/types'

export const MOCK_PASSWORD = 'drydock'
const LOCKOUT_AFTER = 5
const LOCKOUT_SECONDS = 120

export interface MockBackend {
  /** Whether a session exists. */
  signedIn: boolean
  /** No password set: sign-in answers 503 not_configured. */
  notConfigured: boolean
  failures: number
  lockedUntil: number
  devices: Device[]
  /** Every request the harness answered, for specs that assert on what was sent. */
  log: Array<{ method: string; url: string; credentials: RequestCredentials; mode: RequestMode; contentType: string | null }>
}

const CURRENT_ID = 'c0ffee0000000000000000000000000000000000000000000000000000000001'

function sampleDevices(now: number): Device[] {
  const iso = (msAgo: number) => new Date(now - msAgo).toISOString()
  return [
    {
      id: CURRENT_ID, label: 'iPhone Safari', created_ip: '192.168.1.24',
      created_at: iso(3 * 86400e3), last_seen_at: iso(20e3), expires_at: iso(-27 * 86400e3), is_current: true,
    },
    {
      id: 'c0ffee0000000000000000000000000000000000000000000000000000000002', label: 'Mac Firefox', created_ip: '192.168.1.31',
      created_at: iso(12 * 86400e3), last_seen_at: iso(2 * 3600e3), expires_at: iso(-18 * 86400e3), is_current: false,
    },
    {
      id: 'c0ffee0000000000000000000000000000000000000000000000000000000003', label: 'Linux Chrome', created_ip: '192.168.1.40',
      created_at: iso(20 * 86400e3), last_seen_at: iso(9 * 86400e3), expires_at: iso(-10 * 86400e3), is_current: false,
    },
  ]
}

export function newBackend(overrides: Partial<MockBackend> = {}): MockBackend {
  return {
    signedIn: false,
    notConfigured: false,
    failures: 0,
    lockedUntil: 0,
    devices: sampleDevices(Date.now()),
    log: [],
    ...overrides,
  }
}

function envelope(status: number, code: string, message: string, headers: Record<string, string> = {}) {
  return HttpResponse.json({ error: { code, message } }, { status, headers })
}

const unauthenticated = () => envelope(401, 'unauthenticated', 'Sign in to continue.')

/** The handlers, closed over one backend so a spec can reach in and change it. */
export function handlersFor(b: MockBackend): HttpHandler[] {
  const record = (request: Request) => {
    b.log.push({
      method: request.method,
      url: request.url,
      credentials: request.credentials,
      mode: request.mode,
      contentType: request.headers.get('Content-Type'),
    })
  }

  return [
    http.post('/api/auth/session', async ({ request }) => {
      record(request)
      const now = Date.now()
      if (b.lockedUntil > now) {
        const ra = String(Math.ceil((b.lockedUntil - now) / 1000))
        return envelope(429, 'locked_out', 'Too many failed sign-ins. Wait before trying again.', { 'Retry-After': ra })
      }
      let password: unknown
      try {
        password = ((await request.json()) as { password?: unknown }).password
      } catch {
        password = undefined
      }
      if (typeof password !== 'string' || password === '') {
        return envelope(400, 'bad_request', 'Send a JSON body with a password.')
      }
      if (b.notConfigured) {
        return envelope(503, 'not_configured', 'Drydock has no password yet. Run `drydock passwd` on the host.')
      }
      if (password !== MOCK_PASSWORD) {
        b.failures++
        if (b.failures >= LOCKOUT_AFTER) b.lockedUntil = now + LOCKOUT_SECONDS * 1000
        return envelope(401, 'bad_password', 'That password is not right.')
      }
      b.failures = 0
      b.signedIn = true
      if (!b.devices.some((d) => d.is_current)) b.devices = sampleDevices(now)
      return new HttpResponse(null, { status: 204 })
    }),

    http.get('/api/auth/session', ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      const cur = b.devices.find((d) => d.is_current)
      const body: SessionInfo = { current: cur?.id ?? '', devices: b.devices }
      return HttpResponse.json(body, { headers: { 'Cache-Control': 'no-store' } })
    }),

    http.delete('/api/auth/session', ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      const all = new URL(request.url).searchParams.get('all') === 'true'
      b.devices = all ? [] : b.devices.filter((d) => !d.is_current)
      b.signedIn = false
      return new HttpResponse(null, { status: 204 })
    }),

    // The gate, for every other /api path: 401 before anything else, then a
    // JSON 404. Never HTML. (The real server answers its declared-but-unbuilt
    // routes 501; nothing in Phase 1's UI calls one.)
    http.all('/api/*', ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      return envelope(404, 'not_found', 'No such API route.')
    }),
  ]
}
