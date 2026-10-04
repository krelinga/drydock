// A pretend Drydock for the MSW harness (frontend §10): the three
// /api/auth/session routes, GET /api/repos and its refresh, the event stream
// with Last-Event-ID replay and `resync`, and the gate's behaviour for
// everything else,
// shaped exactly as internal/api writes them — the same codes, the same
// statuses, the same Retry-After, the same 401-before-404 ordering for an
// unknown /api path. The specs and `npm run dev:mock` share it, so what the UI
// is developed against is what it is tested against.
//
// State is in memory and per process. There is no cookie: the browser's
// HttpOnly cookie cannot be faked from a service worker, so "signed in" is a
// flag here. That is the one place this harness is not the server, and it is
// why cookie semantics belong to the browser tier (testing §10).

import { http, HttpResponse, sse, type HttpHandler } from 'msw'
import type { CatalogView, Device, InstallationView, RepoView, SessionInfo, StreamEvent, WorkspaceState } from '../api/types'

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

  /** False: GET /api/repos and the refresh answer 503 app_not_configured. */
  appConfigured: boolean
  refreshedAt: string | null
  /** internal/catalog's in-memory failure: set by a failed refresh, cleared by a good one. */
  refreshError: { at: string; message: string } | null
  installations: InstallationView[]
  /** The repository cache, without the workspace join (computed from `workspaces`). */
  repos: Array<Omit<RepoView, 'workspace'>>
  workspaces: Record<string, MockWorkspace>
  /** The event log, oldest first. */
  events: StreamEvent[]
  /** How many events a reconnect may be behind and still be replayed. */
  replayWindow: number
  /** Live subscribers: the browser worker's SSE clients, or a spec's fake EventSource. */
  subscribers: Set<(ev: StreamEvent) => void>
  /** Open SSE connections in the browser harness, so one can be dropped on purpose. */
  streams: Set<{ error(): void; close(): void }>
  /**
   * `auto` finishes a refresh after `refreshDelayMs`, as the server's
   * background goroutine would. `manual` leaves it for a spec to finish with
   * `completeRefresh`, so the in-flight state can be observed.
   */
  refreshMode: 'auto' | 'manual'
  refreshDelayMs: number
  pendingRefreshes: number
}

export interface MockWorkspace {
  id: string
  repository_id: number
  branch: string
  state: WorkspaceState
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

export const SAMPLE_INSTALLATIONS: InstallationView[] = [
  { id: 101, account: 'krelinga', settings_url: 'https://github.com/settings/installations/101' },
]

/** ULIDs, so they sort by creation like the server's. */
export const WS_RUNNING = '01JA0000000000000000000001'
export const WS_FAILED = '01JA0000000000000000000002'
export const WS_REMOVED = '01JA0000000000000000000003'

function sampleRepos(now: number): Array<Omit<RepoView, 'workspace'>> {
  const iso = (msAgo: number) => new Date(now - msAgo).toISOString()
  const r = (id: number, name: string, dc: boolean | null, ago: number, extra: Partial<RepoView> = {}) => ({
    id, installation_id: 101, full_name: name, default_branch: 'main', private: true, archived: false,
    has_devcontainer: dc, pushed_at: iso(ago), removed: false, ...extra,
  })
  return [
    r(1, 'krelinga/drydock', true, 3600e3),
    r(2, 'krelinga/homelab', false, 2 * 86400e3),
    r(3, 'krelinga/notes', null, 5 * 86400e3, { private: false }),
    r(4, 'krelinga/old-site', true, 90 * 86400e3, { archived: true }),
    r(5, 'krelinga/scratch', true, 40 * 86400e3, { removed: true }),
  ]
}

function sampleWorkspaces(): Record<string, MockWorkspace> {
  return {
    [WS_RUNNING]: { id: WS_RUNNING, repository_id: 1, branch: 'main', state: 'running' },
    [WS_FAILED]: { id: WS_FAILED, repository_id: 2, branch: 'main', state: 'failed' },
    [WS_REMOVED]: { id: WS_REMOVED, repository_id: 5, branch: 'main', state: 'stopped' },
  }
}

export function newBackend(overrides: Partial<MockBackend> = {}): MockBackend {
  const now = Date.now()
  return {
    signedIn: false,
    notConfigured: false,
    failures: 0,
    lockedUntil: 0,
    devices: sampleDevices(now),
    log: [],
    appConfigured: true,
    refreshedAt: new Date(now - 600e3).toISOString(),
    refreshError: null,
    installations: SAMPLE_INSTALLATIONS,
    repos: sampleRepos(now),
    workspaces: sampleWorkspaces(),
    events: [],
    replayWindow: 1000,
    subscribers: new Set(),
    streams: new Set(),
    refreshMode: 'auto',
    refreshDelayMs: 1200,
    pendingRefreshes: 0,
    ...overrides,
  }
}

/** GET /api/repos, joined as internal/catalog/view.go joins it. */
export function catalogView(b: MockBackend): CatalogView {
  return {
    refreshed_at: b.refreshedAt,
    last_refresh_error: b.refreshError,
    installations: b.installations,
    repos: b.repos.map((r) => {
      const ws = Object.values(b.workspaces)
        .filter((w) => w.repository_id === r.id && w.state !== 'deleting')
        .sort((x, y) => y.id.localeCompare(x.id))[0]
      return { ...r, workspace: ws ? { id: ws.id, state: ws.state } : null }
    }),
  }
}

/**
 * Appends to the event log and publishes to every subscriber, as
 * internal/events.Log.Append does. Workspace events also move the backend's
 * own rows, so GET /api/repos agrees with the stream.
 */
export function emit(
  b: MockBackend,
  kind: string,
  fields: { workspace_id?: string; level?: StreamEvent['level']; message?: string; data?: Record<string, unknown> } = {},
): StreamEvent {
  const last = b.events[b.events.length - 1]
  const ev: StreamEvent = {
    id: (last?.id ?? 0) + 1,
    level: fields.level ?? 'info',
    kind,
    message: fields.message ?? kind,
    at: new Date().toISOString(),
    ...(fields.workspace_id !== undefined ? { workspace_id: fields.workspace_id } : {}),
    ...(fields.data !== undefined ? { data: fields.data } : {}),
  }
  b.events.push(ev)
  const id = fields.workspace_id
  const d = fields.data ?? {}
  if (id !== undefined && kind === 'workspace.state' && typeof d.state === 'string') {
    const cur = b.workspaces[id]
    const repository_id = typeof d.repository_id === 'number' ? d.repository_id : cur?.repository_id
    if (repository_id !== undefined) {
      b.workspaces[id] = { id, repository_id, branch: String(d.branch ?? cur?.branch ?? 'main'), state: d.state as WorkspaceState }
    }
  }
  if (id !== undefined && kind === 'workspace.gone') delete b.workspaces[id]
  for (const s of b.subscribers) s(ev)
  return ev
}

/** Finishes one pending refresh, the way internal/catalog reports it. */
export function completeRefresh(b: MockBackend, ok = true): StreamEvent {
  b.pendingRefreshes = Math.max(0, b.pendingRefreshes - 1)
  if (!ok) {
    const message = 'Could not refresh the repository list from GitHub: GitHub answered 401 (Bad credentials).'
    b.refreshError = { at: new Date().toISOString(), message }
    return emit(b, 'repo.refresh_failed', { level: 'warn', data: {}, message })
  }
  b.refreshError = null
  b.refreshedAt = new Date().toISOString()
  return emit(b, 'repo.refreshed', {
    message: `Repository list refreshed: ${b.repos.length} repositories.`,
    data: { count: b.repos.length, added: 0, removed: 0 },
  })
}

/**
 * The events a clone emits, in the order internal/workspace writes them: the
 * create, then each step's started/done around the state moves. `failAt`
 * ends it with that step failed and the workspace failed, naming the step.
 */
export function cloneScript(
  b: MockBackend, repositoryId: number, id: string, failAt?: string,
): Array<() => void> {
  const st = (state: WorkspaceState, data: Record<string, unknown> = {}, level: StreamEvent['level'] = 'info') =>
    () => { emit(b, 'workspace.state', { workspace_id: id, level, data: { state, ...data } }) }
  const step = (name: string, status: string, detail?: string) =>
    () => { emit(b, 'workspace.step', { workspace_id: id, level: status === 'failed' ? 'error' : 'info', data: { step: name, status, ...(detail ? { detail } : {}) } }) }
  const out: Array<() => void> = [st('pending', { repository_id: repositoryId, branch: 'main' })]
  const plan: Array<[string, WorkspaceState | null]> = [
    ['allocate', null], ['clone', 'cloning'], ['resolve_config', null], ['credential_volume', null],
    ['broker_socket', null], ['up', 'building'], ['verify', null],
  ]
  let from: WorkspaceState = 'pending'
  for (const [name, enter] of plan) {
    if (enter !== null) {
      out.push(st(enter, { from }))
      from = enter
    }
    out.push(step(name, 'started'))
    if (name === failAt) {
      const detail = `The ${name.replace('_', ' ')} step failed.`
      out.push(step(name, 'failed', detail), st('failed', { from, detail }, 'error'))
      return out
    }
    out.push(step(name, 'done'))
  }
  out.push(st('running', { from }))
  return out
}

function envelope(status: number, code: string, message: string, headers: Record<string, string> = {}) {
  return HttpResponse.json({ error: { code, message } }, { status, headers })
}

const unauthenticated = () => envelope(401, 'unauthenticated', 'Sign in to continue.')
const appNotConfigured = () =>
  envelope(503, 'app_not_configured', 'No GitHub App is configured, so there is no repository list.')

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

    http.get('/api/repos', ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      if (!b.appConfigured) return appNotConfigured()
      return HttpResponse.json(catalogView(b), { headers: { 'Cache-Control': 'no-store' } })
    }),

    http.post('/api/repos/refresh', ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      if (!b.appConfigured) return appNotConfigured()
      // Concurrent refreshes are joined, as Catalog.Trigger joins them.
      if (b.pendingRefreshes === 0) {
        b.pendingRefreshes = 1
        if (b.refreshMode === 'auto') setTimeout(() => completeRefresh(b), b.refreshDelayMs)
      }
      return HttpResponse.json({}, { status: 202 })
    }),

    // The stream. The gate first, as internal/api runs it: a 401 here is
    // what closes an EventSource for good (frontend §2.3).
    http.get('/api/events', ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      return undefined // fall through to the stream itself
    }),
    sse<{ message: string; resync: string }>('/api/events', ({ request, client }) => {
      // Replay after Last-Event-ID, or a named resync when the gap is wider
      // than the window — events_routes.go's rules.
      // The header, or the query a hard retry carries; the header wins, as
      // on the server (internal/api/events_routes.go).
      const h = request.headers.get('Last-Event-ID') ?? new URL(request.url).searchParams.get('last_event_id')
      if (h !== null) {
        const last = Number(h)
        const first = b.events[0]?.id ?? 1
        const behind = b.events.filter((e) => e.id > last)
        const latest = b.events[b.events.length - 1]?.id ?? 0
        if (!Number.isInteger(last) || last < 0 || (behind.length > 0 && last < first - 1) || behind.length > b.replayWindow) {
          client.send({ id: String(latest), event: 'resync', data: '{}' })
        } else {
          for (const e of behind) client.send({ id: String(e.id), data: JSON.stringify(e) })
        }
      }
      const sub = (e: StreamEvent) => {
        if (!b.signedIn) {
          client.close()
          b.subscribers.delete(sub)
          return
        }
        client.send({ id: String(e.id), data: JSON.stringify(e) })
      }
      b.subscribers.add(sub)
      b.streams.add(client)
      request.signal.addEventListener('abort', () => {
        b.subscribers.delete(sub)
        b.streams.delete(client)
      })
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
