// A pretend Drydock for the MSW harness (frontend §10): the three
// /api/auth/session routes, GET /api/repos and its refresh, the workspace
// routes (list, read, create, start, and Phase 6's stop, rebuild and delete,
// a delete that sticks and its resume included), the four /api/secrets
// routes with every refusal they can answer, GET /api/auth/claude and its
// check (the identity half; the login half is null), the event stream with
// Last-Event-ID replay and `resync`, and the gate's behaviour for everything
// else, shaped exactly as internal/api writes them — the same codes, the same
// statuses, the same Retry-After, the same 401-before-404 ordering for an
// unknown /api path. The specs and `npm run dev:mock` share it, so what the UI
// is developed against is what it is tested against.
//
// State is in memory and per process. There is no cookie: the browser's
// HttpOnly cookie cannot be faked from a service worker, so "signed in" is a
// flag here. That is the one place this harness is not the server, and it is
// why cookie semantics belong to the browser tier (testing §10).

import { http, HttpResponse, sse, type HttpHandler } from 'msw'
import type {
  ActionView, CatalogView, Device, IdentityState, IdentityView, InstallationView, PutSecretResult, RepoView, SecretMeta, SessionInfo, SessionView, Stale, StaleWorkspace, SupervisorView,
  StepView, StreamEvent, Undeliverable, UndeliverableSecret, WorkspaceDetail, WorkspaceList, WorkspaceState, WorkspaceView,
} from '../api/types'
import { checkDescription, checkName, checkReach, checkValue, type SecretRefusal } from '../lib/secretRules'

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

  /**
   * How many workspaces may hold or build a container at once: the cap
   * `POST /api/workspaces` answers `409 at_capacity` against.
   */
  capacity: number
  /**
   * `auto` plays a create's or a start's events on a timer, one per
   * `scriptIntervalMs`, as the server's own goroutine would — the create's
   * first event before the 202, since the server commits the row and its
   * event together. `manual` plays nothing: the whole script lands in
   * `scripts` for a spec to play, so the in-flight state can be observed.
   */
  scriptMode: 'auto' | 'manual'
  scriptIntervalMs: number
  scripts: Record<string, Array<() => void>>
  /** The step the next create, start or rebuild fails at, once (dev:mock's `failNext`). */
  failNext: string | null
  /**
   * The sub-step the next stop or delete fails at, once (dev:mock's
   * `failAction`): a stop that leaves the workspace running, or a delete that
   * sticks in `deleting` until it is asked again.
   */
  failAction: string | null
  /**
   * The job each workspace has in flight, as internal/provision's `active`
   * map: one per workspace, so a stop or rebuild on a busy one is `409
   * in_progress`, and a delete joins a delete or cancels anything else.
   */
  jobs: Record<string, 'run' | 'stop' | 'delete'>
  /** Each auto-mode job's pending timers, so a delete can cancel the run it replaces. */
  timers: Record<string, Array<ReturnType<typeof setTimeout>>>
  /** Hands out increasing ULID-shaped ids. */
  idSeq: number

  /** False: every /api/secrets route answers 503 secrets_not_configured, as with no --secrets-key. */
  secretsKey: boolean
  /** The secret table. `value` is the server's own; no route returns it. */
  secrets: Record<string, MockSecret>
  /**
   * The raw body of every write to /api/secrets, for the one spec that must
   * prove a value *was* sent before proving it is nowhere afterwards.
   */
  secretBodies: Array<{ method: string; url: string; body: string }>
  /**
   * The next secret write is refused with this envelope, once, whatever it
   * sent — how a spec (or dev:mock's `refuseSecret`) reaches a server-side
   * refusal the form would have caught first.
   */
  refuseNextSecret: { status: number; code: string; message: string; detail?: string } | null
  /**
   * Workspaces internal/secrets' StaleKind hook calls
   * `needs_supervisor_restart`. Empty until Phase 5, as on the server.
   */
  staleRestart: string[]
  /**
   * The delivery condition internal/secrets' snapshot holds: null while every
   * stored secret can be delivered. GET /api/secrets reports it as is.
   */
  undeliverable: Undeliverable | null
  /**
   * Phase 5's session supervisor: when true, a workspace that reaches
   * running hands off at step 8 and its server plays starting → serving with
   * an environment and `Capacity: 1/4`, the views carry `supervisor`,
   * `session` and `environment_id`, and POST …/supervisor and GET …/logs
   * answer. False (the specs' default) is a server older than Phase 5,
   * whose views carry none of the three.
   */
  supervisor: boolean

  /**
   * The stored Claude identity internal/identity.Watch holds. Seeded ok and a
   * month from expiry, so a spec about something else sees no banner; a spec
   * moves it with `setIdentity`, which emits auth.identity as the watch does.
   */
  identity: IdentityView
  /** How many POST /api/auth/claude/check requests arrived. */
  identityChecks: number
}

/** A stored identity as the watch writes it: login details only beside a login. */
export function identityView(state: IdentityState | null, expiresInMs = 30 * 86400e3): IdentityView {
  const live = state === 'ok' || state === 'expiring' || state === 'expired'
  const now = Date.now()
  return {
    state,
    account_email: live ? 'operator@example.invalid' : null,
    expires_at: live ? new Date(now + expiresInMs).toISOString() : null,
    logged_in_at: live ? new Date(now - 20 * 86400e3).toISOString() : null,
    last_checked_at: state === null ? null : new Date(now - 60e3).toISOString(),
    volume: 'drydock-claude-config',
    check_error: null,
  }
}

/** Stores a new identity and announces it, as a check that saw it would. */
export function setIdentity(b: MockBackend, view: IdentityView): StreamEvent {
  b.identity = view
  const level = view.state === 'blanked' || view.state === 'expired' ? 'error' : view.state === 'expiring' ? 'warn' : 'info'
  return emit(b, 'auth.identity', { level, message: `Claude identity: ${view.state}`, data: { identity: view } })
}

/** A check that could not read its inputs: the stored state stands. */
export function failIdentityCheck(b: MockBackend, message = 'Could not check the Claude login: Docker did not answer. The last known state is kept.'): StreamEvent {
  const check_error = { at: new Date().toISOString(), problem: 'docker', message }
  b.identity = { ...b.identity, check_error, last_checked_at: check_error.at }
  return emit(b, 'auth.identity_check_failed', { level: 'warn', message, data: { check_error } })
}

export interface MockSecret {
  name: string
  value: string
  reach: string
  description: string
  all_repos: boolean
  grants: number[]
  created_at: string
  rotated_at: string | null
  last_access_at: string | null
  accessed_by: string[]
}

export interface MockWorkspace {
  id: string
  repository_id: number
  branch: string
  state: WorkspaceState
  state_detail: string | null
  container_id: string | null
  created_at: string
  steps: Record<string, StepView>
  /** The latest workspace.action event, as internal/workspace's view reads it; absent is none. */
  last_action?: ActionView | null
  /** The latest supervisor.state and session.status, as the view reads them back. */
  supervisor?: SupervisorView | null
  session?: SessionView | null
  environment_id?: string | null
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

/**
 * The sample workspaces, made the way the server makes them: by playing
 * their events into the log, so each one's row, steps and history agree.
 * WS_RUNNING cloned and came up; WS_FAILED failed at `up`; WS_REMOVED ran and
 * was stopped. Nothing is subscribed yet, so this publishes to no one.
 */
function seedWorkspaces(b: MockBackend, now: number): void {
  const plays: Array<[string, number, string | undefined, number]> = [
    [WS_RUNNING, 1, undefined, 3 * 3600e3],
    [WS_FAILED, 2, 'up', 26 * 3600e3],
    [WS_REMOVED, 5, undefined, 9 * 86400e3],
  ]
  // Oldest first, so the log's ids run in time order like the server's.
  for (const [id, repo, failAt, ago] of plays.sort((x, y) => y[3] - x[3])) {
    cloneScript(b, repo, id, failAt).forEach((play, i) => play(new Date(now - ago + i * 4000).toISOString()))
    if (id === WS_REMOVED) {
      emit(b, 'workspace.state', {
        workspace_id: id, at: new Date(now - ago + 3600e3).toISOString(),
        message: 'Stopped.', data: { state: 'stopped', from: 'running' },
      })
    }
  }
}

export function newBackend(overrides: Partial<MockBackend> = {}): MockBackend {
  const now = Date.now()
  const b = newBackendBare(now, overrides)
  if (overrides.workspaces === undefined && overrides.events === undefined) seedWorkspaces(b, now)
  return b
}

function newBackendBare(now: number, overrides: Partial<MockBackend>): MockBackend {
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
    workspaces: {},
    events: [],
    replayWindow: 1000,
    subscribers: new Set(),
    streams: new Set(),
    refreshMode: 'auto',
    refreshDelayMs: 1200,
    pendingRefreshes: 0,
    capacity: 4,
    scriptMode: 'auto',
    scriptIntervalMs: 900,
    scripts: {},
    failNext: null,
    failAction: null,
    jobs: {},
    timers: {},
    idSeq: 0,
    secretsKey: true,
    secrets: sampleSecrets(now),
    secretBodies: [],
    refuseNextSecret: null,
    staleRestart: [],
    undeliverable: null,
    identity: identityView('ok'),
    identityChecks: 0,
    supervisor: false,
    ...overrides,
  }
}

/**
 * Three secrets, one per grant shape: one repository (and fetched by its
 * running workspace), every repository, and nothing at all — default deny.
 * Seeded as rows without events, as a database from an earlier run would be.
 */
function sampleSecrets(now: number): Record<string, MockSecret> {
  const iso = (msAgo: number) => new Date(now - msAgo).toISOString()
  const list: MockSecret[] = [
    {
      name: 'NPM_READ_TOKEN', value: 'npm_mock_value_not_a_real_token',
      reach: 'Read packages from the private npm registry. It cannot publish.',
      description: 'npm automation token, read-only. Rotate at npmjs.com → Access Tokens.',
      all_repos: true, grants: [], created_at: iso(30 * 86400e3), rotated_at: null,
      last_access_at: iso(5 * 60e3), accessed_by: [WS_RUNNING],
    },
    {
      name: 'STAGING_DB_URL', value: 'postgres://mock@staging.invalid/app',
      reach: 'Read and write the staging database, which holds synthetic data only.',
      description: '', all_repos: false, grants: [], created_at: iso(2 * 86400e3), rotated_at: null,
      last_access_at: null, accessed_by: [],
    },
    {
      name: 'STRIPE_TEST_KEY', value: 'sk_test_mock_value',
      reach: 'Create charges and customers in the Stripe test account. Test mode only: no real money moves.',
      description: 'Stripe dashboard → Developers → API keys (test mode). Roll it there, then paste the new one here.',
      all_repos: false, grants: [1], created_at: iso(10 * 86400e3), rotated_at: iso(3 * 86400e3),
      last_access_at: iso(90e3), accessed_by: [WS_RUNNING],
    },
  ]
  return Object.fromEntries(list.map((s) => [s.name, s]))
}

/** internal/secrets.Meta for a row: everything but the value. */
export function secretMeta(b: MockBackend, s: MockSecret): SecretMeta {
  return {
    name: s.name,
    reach: s.reach,
    description: s.description,
    all_repos: s.all_repos,
    grants: [...s.grants].sort((x, y) => x - y).map((id) => ({
      repository_id: id, full_name: b.repos.find((r) => r.id === id)?.full_name ?? '',
    })),
    created_at: s.created_at,
    rotated_at: s.rotated_at,
    last_access_at: s.last_access_at,
    accessed_by: [...s.accessed_by],
  }
}

/** The running workspaces a secret reaches, by kind — internal/secrets' `stale`. */
function staleFor(b: MockBackend, s: MockSecret): Stale {
  const out: Stale = { new_commands: [], needs_supervisor_restart: [] }
  const running = Object.values(b.workspaces)
    .filter((w) => w.state === 'running' && (s.all_repos || s.grants.includes(w.repository_id)))
    .sort((x, y) => x.id.localeCompare(y.id))
  for (const w of running) {
    const sw: StaleWorkspace = {
      workspace_id: w.id, repository_id: w.repository_id,
      full_name: b.repos.find((r) => r.id === w.repository_id)?.full_name ?? '',
    }
    if (b.staleRestart.includes(w.id)) out.needs_supervisor_restart.push(sw)
    else out.new_commands.push(sw)
  }
  return out
}

/**
 * A workspace fetched the secrets granted to it, as GET-SECRETS records it:
 * a `secret_access` row per secret and no event. Only a refetch of the list
 * shows it.
 */
export function recordSecretFetch(b: MockBackend, workspaceId: string): string[] {
  const w = b.workspaces[workspaceId]
  if (w === undefined) return []
  const at = new Date().toISOString()
  const got = Object.values(b.secrets).filter((s) => s.all_repos || s.grants.includes(w.repository_id))
  for (const s of got) {
    s.last_access_at = at
    s.accessed_by = [workspaceId, ...s.accessed_by.filter((x) => x !== workspaceId)]
  }
  return got.map((s) => s.name)
}

/**
 * Stored secrets stop being deliverable (internal/secrets noteLocked): the
 * named rows join the condition GET /api/secrets reports, and the change is
 * announced. By default the master key no longer opens STRIPE_TEST_KEY — what
 * a replaced key does — so storing its value again repairs it.
 */
export function secretUndeliverable(
  b: MockBackend,
  rows: UndeliverableSecret[] = [{ name: 'STRIPE_TEST_KEY', reason: 'does_not_open' }],
): StreamEvent {
  const known = b.undeliverable?.secrets ?? []
  const secrets = [...known, ...rows.filter((r) => !known.some((k) => k.name === r.name))]
    .sort((x, y) => x.name.localeCompare(y.name))
  b.undeliverable = { since: b.undeliverable?.since ?? new Date().toISOString(), secrets }
  return emit(b, 'secret.undeliverable', {
    level: 'error',
    message: `Stored secrets cannot be delivered, so every workspace's commands will fail until this is fixed: ${
      secrets.map((s) => `secrets: secret ${s.name} ${s.reason === 'does_not_open' ? 'does not decrypt under this master key' : 'fails the write-time rules'}`).join('; ')}`,
    data: { undeliverable: b.undeliverable },
  })
}

/**
 * A write that repairs a row: a PUT with a value repairs one the key could
 * not open, a delete repairs either kind. The last repair is
 * secret.deliverable; one that leaves others broken is a new report.
 */
function repairDelivery(b: MockBackend, name: string, onlyReason?: string): void {
  const u = b.undeliverable
  if (u === null || !u.secrets.some((s) => s.name === name && (onlyReason === undefined || s.reason === onlyReason))) return
  const rest = u.secrets.filter((s) => s.name !== name)
  if (rest.length === 0) {
    b.undeliverable = null
    emit(b, 'secret.deliverable', { message: 'Stored secrets can be delivered again.', data: {} })
    return
  }
  b.undeliverable = { since: u.since, secrets: [] }
  secretUndeliverable(b, rest)
}

/** A fresh ULID-shaped id that sorts after every sample and every earlier one. */
export function nextWorkspaceId(b: MockBackend): string {
  b.idSeq++
  return `01JC${String(b.idSeq).padStart(22, '0')}`
}

/** States that count against the cap: what is building or running. */
const OCCUPYING = new Set<WorkspaceState>(['pending', 'cloning', 'building', 'running'])

/** One row as `GET /api/workspaces` writes it. */
export function workspaceView(b: MockBackend, w: MockWorkspace): WorkspaceView {
  return {
    id: w.id,
    repository_id: w.repository_id,
    full_name: b.repos.find((r) => r.id === w.repository_id)?.full_name ?? '',
    branch: w.branch,
    state: w.state,
    state_detail: w.state_detail,
    container_id: w.container_id,
    created_at: w.created_at,
    steps: structuredClone(w.steps),
    last_action: w.last_action ? { ...w.last_action } : null,
    ...(b.supervisor ? {
      supervisor: w.supervisor ? { ...w.supervisor } : null,
      session: w.session ? { ...w.session } : null,
      environment_id: w.environment_id ?? null,
    } : {}),
  }
}

/** The cap and the occupied count, as internal/workspace CapacityOf counts the list's rows. */
export function capacityView(b: MockBackend): { cap: number; occupied: number } {
  return { cap: b.capacity, occupied: Object.values(b.workspaces).filter((w) => OCCUPYING.has(w.state)).length }
}

/** `GET /api/workspaces`: every row, newest first. */
export function workspaceList(b: MockBackend): WorkspaceList {
  return {
    workspaces: Object.values(b.workspaces)
      .sort((x, y) => y.id.localeCompare(x.id))
      .map((w) => workspaceView(b, w)),
    capacity: capacityView(b),
  }
}

/** `GET /api/workspaces/:id`: the row and its latest 50 events, newest first. */
export function workspaceDetail(b: MockBackend, w: MockWorkspace): WorkspaceDetail {
  const events = b.events.filter((e) => e.workspace_id === w.id).slice(-50).reverse()
  return { ...workspaceView(b, w), events }
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
  fields: {
    workspace_id?: string; level?: StreamEvent['level']; message?: string; data?: Record<string, unknown>
    /** Backdates the event; the seed uses it. */
    at?: string
  } = {},
): StreamEvent {
  const last = b.events[b.events.length - 1]
  const ev: StreamEvent = {
    id: (last?.id ?? 0) + 1,
    level: fields.level ?? 'info',
    kind,
    message: fields.message ?? kind,
    at: fields.at ?? new Date().toISOString(),
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
      const state = d.state as WorkspaceState
      const row: MockWorkspace = cur ?? {
        id, repository_id, branch: 'main', state, state_detail: null, container_id: null, created_at: ev.at, steps: {},
      }
      b.workspaces[id] = {
        ...row,
        repository_id,
        branch: String(d.branch ?? row.branch),
        state,
        state_detail: typeof d.detail === 'string' ? d.detail : null,
        // The move to running carries the container id, as the server's does
        // (design §6); a create starts clean.
        container_id: typeof d.container_id === 'string' ? d.container_id
          : state === 'pending' ? null : row.container_id,
        steps: state === 'pending' ? {} : row.steps,
        created_at: state === 'pending' && cur === undefined ? ev.at : row.created_at,
      }
    }
  }
  if (id !== undefined && kind === 'workspace.step' && typeof d.step === 'string') {
    const cur = b.workspaces[id]
    if (cur !== undefined) {
      const step: StepView = { status: d.status as StepView['status'], at: ev.at }
      if (typeof d.detail === 'string') step.detail = d.detail
      b.workspaces[id] = { ...cur, steps: { ...cur.steps, [d.step]: step } }
    }
  }
  if (id !== undefined && kind === 'workspace.action' && typeof d.step === 'string' && typeof d.action === 'string') {
    const cur = b.workspaces[id]
    if (cur !== undefined) {
      const last: ActionView = { action: d.action, step: d.step, status: d.status as ActionView['status'], at: ev.at }
      if (typeof d.detail === 'string') last.detail = d.detail
      b.workspaces[id] = { ...cur, last_action: last }
    }
  }
  if (id !== undefined && kind === 'workspace.adopted' && typeof d.container_id === 'string') {
    const cur = b.workspaces[id]
    if (cur !== undefined) b.workspaces[id] = { ...cur, container_id: d.container_id }
  }
  if (id !== undefined && kind === 'supervisor.state') {
    const cur = b.workspaces[id]
    if (cur !== undefined) b.workspaces[id] = { ...cur, supervisor: { ...(d as unknown as SupervisorView), at: ev.at } }
  }
  if (id !== undefined && kind === 'session.status') {
    const cur = b.workspaces[id]
    if (cur !== undefined) {
      const env = typeof d.environment_id === 'string' ? d.environment_id : cur.environment_id ?? null
      b.workspaces[id] = { ...cur, session: { ...(d as unknown as SessionView), at: ev.at }, environment_id: env }
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
  b: MockBackend, repositoryId: number, id: string, failAt?: string, branch = 'main',
): Array<(at?: string) => void> {
  const steps = new ScriptSteps(b, id)
  const out: Array<(at?: string) => void> = [
    steps.state('pending', { repository_id: repositoryId, branch }, 'Workspace created.'),
  ]
  const plan: Array<[string, WorkspaceState | null]> = [
    ['allocate', null], ['clone', 'cloning'], ['resolve_config', null], ['credential_volume', null],
    ['broker_socket', null], ['up', 'building'], ['verify', null],
  ]
  // Phase 2 stops at verify: session_server is Phase 5's supervisor.
  return steps.run(out, plan, 'pending', failAt)
}

/**
 * The events a start emits, as internal/provision's Start runs it: from step
 * 3 (resolve_config) in `building` when the clone's last step said done, and
 * from step 2 in `cloning` otherwise. Steps before that are not rerun, so
 * their rows keep the earlier run's events — and a step the earlier run
 * reached past this run's failure keeps its old status too, which is what
 * the UI's runSteps exists to leave out.
 */
export function startScript(b: MockBackend, id: string, failAt?: string): Array<(at?: string) => void> {
  const w = b.workspaces[id]
  const from = w?.state ?? 'stopped'
  const steps = new ScriptSteps(b, id)
  const rest: Array<[string, WorkspaceState | null]> = [['credential_volume', null], ['broker_socket', null], ['up', null], ['verify', null]]
  const plan: Array<[string, WorkspaceState | null]> = w?.steps.clone?.status === 'done'
    ? [['resolve_config', 'building'], ...rest]
    : [['clone', 'cloning'], ['resolve_config', null], ...rest.map(([n]): [string, WorkspaceState | null] => [n, n === 'up' ? 'building' : null])]
  return steps.run([], plan, from, failAt)
}

/** A stop's and a delete's sub-steps (internal/provision lifecycle.go), in order. */
const STOP_SUBSTEPS = ['session_server', 'container', 'broker_socket']
const DELETE_SUBSTEPS = ['session_server', 'containers', 'broker_socket', 'files']

/** What a sub-step says when it fails: lifecycle.go's `workspace.Public` sentences. */
const ACTION_FAILED: Record<string, string> = {
  session_server: 'Drydock could not stop the session server.',
  container: "docker could not stop the workspace's container.",
  containers: "A container carrying the workspace's label is still there.",
  broker_socket: "Drydock could not close the workspace's GitHub access socket.",
  files: "Drydock could not remove the workspace's directory; files inside may belong to another user.",
}

/** The note a sub-step that did nothing, or something worth saying, ends `done` with. */
function actionNote(b: MockBackend, id: string, step: string): string | undefined {
  if (step === 'session_server') {
    return b.supervisor
      ? 'Stopped the session server, SIGTERM first, so its environment is kept for the next start.'
      : 'Nothing to do yet: the Claude Code session server arrives with Claude support.'
  }
  if (step === 'containers') return b.workspaces[id]?.container_id ? 'Removed its container.' : 'No container to remove.'
  return undefined
}

/** internal/provision StopFailedDetail: the annotation a failed stop leaves. */
export function stopFailedDetail(pub: string): string {
  return `The stop did not finish: ${pub} Stop again to retry.`
}

/**
 * The events a stop emits, as internal/provision's stopJob writes them: each
 * sub-step started and done, then running → stopped. `failAt` ends it with
 * that sub-step failed and the workspace still running, annotated with the
 * sub-step's sentence (Annotate: a running → running state event).
 */
export function stopScript(b: MockBackend, id: string, failAt?: string): Array<(at?: string) => void> {
  const steps = new ScriptSteps(b, id)
  const out: Array<(at?: string) => void> = []
  for (const sub of STOP_SUBSTEPS) {
    out.push(steps.action('stop', sub, 'started'))
    if (sub === failAt) {
      const pub = ACTION_FAILED[sub] ?? 'The step failed.'
      const detail = stopFailedDetail(pub)
      out.push(
        steps.action('stop', sub, 'failed', pub),
        steps.state('running', { from: 'running', detail }, `Running. ${detail}`, 'warn'),
      )
      return out
    }
    out.push(steps.action('stop', sub, 'done', actionNote(b, id, sub)))
    if (sub === 'session_server' && b.supervisor) out.push(steps.supervisor('exited', 'stopped', 'The session server was stopped.'))
  }
  out.push(steps.state('stopped', { from: 'running' }, 'Stopped.'))
  return out
}

/**
 * The events a delete emits, as internal/provision's startDelete and
 * deleteJob write them: the move to deleting (not on a resume, which finds
 * the workspace there already), each sub-step, then `workspace.gone`.
 * `failAt` sticks it: the sub-step fails, and Annotate writes a deleting →
 * deleting state event whose detail names it. Asking again resumes.
 */
export function deleteScript(b: MockBackend, id: string, from: WorkspaceState | null, failAt?: string): Array<(at?: string) => void> {
  const steps = new ScriptSteps(b, id)
  const out: Array<(at?: string) => void> = from === null ? [] : [steps.state('deleting', { from }, 'Deleting.')]
  for (const sub of DELETE_SUBSTEPS) {
    out.push(steps.action('delete', sub, 'started'))
    if (sub === failAt) {
      const pub = ACTION_FAILED[sub] ?? 'The step failed.'
      const detail = `The delete stopped part-way: ${pub} Delete again to retry.`
      out.push(
        steps.action('delete', sub, 'failed', pub),
        steps.state('deleting', { from: 'deleting', detail }, `Deleting. ${detail}`, 'warn'),
      )
      return out
    }
    out.push(steps.action('delete', sub, 'done', actionNote(b, id, sub)))
  }
  out.push((at?: string) => {
    emit(b, 'workspace.gone', { workspace_id: id, message: 'Workspace deleted.', data: {}, ...(at ? { at } : {}) })
  })
  return out
}

/** The id `up` reports for a workspace's container: deterministic, so specs can name it. */
export function mockContainerId(id: string): string {
  return `c0ffee${id.slice(-10).toLowerCase()}${'0'.repeat(48)}`.slice(0, 64)
}

/** What a failed step says: a `workspace.Public` sentence, never a raw error. */
const FAILED_SENTENCE: Record<string, string> = {
  allocate: 'Could not allocate the workspace directory.',
  clone: 'Could not clone the repository from GitHub.',
  resolve_config: 'Could not read the dev container configuration.',
  credential_volume: 'Could not prepare the credential volume.',
  broker_socket: 'Could not open the broker socket.',
  up: 'The container did not start. The service log has the build output.',
  verify: 'The container started but did not pass its checks.',
}

class ScriptSteps {
  constructor(private b: MockBackend, private id: string) {}

  state(state: WorkspaceState, data: Record<string, unknown>, message: string, level: StreamEvent['level'] = 'info') {
    return (at?: string) => {
      emit(this.b, 'workspace.state', { workspace_id: this.id, level, message, data: { state, ...data }, ...(at ? { at } : {}) })
    }
  }

  action(action: string, step: string, status: string, detail?: string) {
    const word = action.charAt(0).toUpperCase() + action.slice(1)
    const message = detail ? `${word}: ${step} ${status}. ${detail}` : `${word}: ${step} ${status}.`
    return (at?: string) => {
      emit(this.b, 'workspace.action', {
        workspace_id: this.id, level: status === 'failed' ? 'error' : 'info', message,
        data: { action, step, status, ...(detail ? { detail } : {}) }, ...(at ? { at } : {}),
      })
    }
  }

  step(name: string, status: string, detail?: string) {
    const message = status === 'failed' ? detail ?? `Step ${name} failed.` : `Step ${name} ${status}.`
    return (at?: string) => {
      emit(this.b, 'workspace.step', {
        workspace_id: this.id, level: status === 'failed' ? 'error' : 'info', message,
        data: { step: name, status, ...(detail ? { detail } : {}) }, ...(at ? { at } : {}),
      })
    }
  }

  supervisor(state: string, reason: string, detail: string, level: StreamEvent['level'] = 'info') {
    return (at?: string) => {
      const prev = this.b.workspaces[this.id]?.supervisor
      emit(this.b, 'supervisor.state', {
        workspace_id: this.id, level, message: detail || `The session server is ${state}.`,
        data: { state, from: prev?.state ?? '', reason, detail, restart_count: prev?.restart_count ?? 0 },
        ...(at ? { at } : {}),
      })
    }
  }

  session(used: number) {
    return (at?: string) => {
      const env = mockEnvironmentId(this.id)
      emit(this.b, 'session.status', {
        workspace_id: this.id, message: `Capacity ${used}/4.`,
        data: { environment_id: env, url: `https://claude.ai/code?environment=${env}`, capacity_used: used, capacity_total: 4, sessions: used },
        ...(at ? { at } : {}),
      })
    }
  }

  /** §6 step 8 and §8: the hand-off, then the server starting and serving. */
  serve(out: Array<(at?: string) => void>): void {
    out.push(
      this.step('session_server', 'started'),
      this.step('session_server', 'done', 'Handed to the session supervisor, which starts the Claude Code session server.'),
      this.supervisor('starting', 'launching', 'Starting the session server.'),
      this.supervisor('serving', 'connected', ''),
      this.session(1),
    )
  }

  run(
    out: Array<(at?: string) => void>, plan: Array<[string, WorkspaceState | null]>, from: WorkspaceState, failAt?: string,
  ): Array<(at?: string) => void> {
    const said: Partial<Record<WorkspaceState, string>> = { cloning: 'Cloning.', building: 'Building the container.' }
    for (const [name, enter] of plan) {
      if (enter !== null) {
        out.push(this.state(enter, { from }, said[enter] ?? enter))
        from = enter
      }
      out.push(this.step(name, 'started'))
      if (name === failAt) {
        const detail = FAILED_SENTENCE[name] ?? `The ${name} step failed.`
        out.push(this.step(name, 'failed', detail), this.state('failed', { from, detail }, `Failed. ${detail}`, 'error'))
        return out
      }
      out.push(this.step(name, 'done'))
    }
    out.push(this.state('running', { from, container_id: mockContainerId(this.id) }, 'Running.'))
    if (this.b.supervisor) this.serve(out)
    return out
  }
}

/** The environment a mock workspace's server advertises: stable per workspace, as the real one is. */
export function mockEnvironmentId(id: string): string {
  return `env_01${id.slice(-12).replace(/[^A-Za-z0-9]/g, '')}`
}

/** A session server restart (POST …/supervisor), as internal/supervisor's Restart writes it. */
export function supervisorScript(b: MockBackend, id: string): Array<(at?: string) => void> {
  const steps = new ScriptSteps(b, id)
  return [
    steps.supervisor('exited', 'stopped', 'The session server was stopped.'),
    steps.supervisor('starting', 'launching', 'Starting the session server.'),
    steps.supervisor('serving', 'connected', ''),
    steps.session(1),
  ]
}

/**
 * Plays a script as the server would: its first event now — the server
 * commits the transition and its event before answering 202 — and the rest
 * from a goroutine. In `manual` mode all of it is held for a spec. The
 * workspace's job is `kind` until the script's last event has played.
 */
function schedule(b: MockBackend, id: string, script: Array<(at?: string) => void>, kind: 'run' | 'stop' | 'delete' = 'run'): void {
  b.jobs[id] = kind
  const last = script.length - 1
  if (last >= 0) {
    const end = script[last]!
    script[last] = (at?: string) => {
      end(at)
      if (b.jobs[id] === kind) delete b.jobs[id]
      delete b.timers[id]
    }
  } else {
    delete b.jobs[id]
  }
  if (b.scriptMode === 'manual') {
    b.scripts[id] = script
    return
  }
  script[0]?.()
  b.timers[id] = script.slice(1).map((play, i) => setTimeout(() => play(), (i + 1) * b.scriptIntervalMs))
}

/** Ends a workspace's job where it stands: what a delete's cancel does to a run. */
function cancelJob(b: MockBackend, id: string): void {
  for (const t of b.timers[id] ?? []) clearTimeout(t)
  delete b.timers[id]
  delete b.scripts[id]
  delete b.jobs[id]
}

/**
 * A retry's ClearDetail: the annotation a failed stop or a stuck delete left
 * goes as the retry starts, before the 202, as the server clears it under the
 * lock before launching the job (frontend §4.5 #15, #16).
 */
function clearDetail(b: MockBackend, id: string, state: WorkspaceState): void {
  const w = b.workspaces[id]
  if (w === undefined || w.state !== state || w.state_detail === null) return
  const said: Partial<Record<WorkspaceState, string>> = { running: 'Running.', deleting: 'Deleting.' }
  emit(b, 'workspace.state', { workspace_id: id, message: said[state] ?? state, data: { state, from: state } })
}

/** Starts a stop, as the route does — also how dev:mock plays one "from another device". */
export function scheduleStop(b: MockBackend, id: string): void {
  clearDetail(b, id, 'running')
  const failAt = b.failAction ?? undefined
  b.failAction = null
  schedule(b, id, stopScript(b, id, failAt), 'stop')
}

/** Starts a delete, or resumes a stuck one; joins one already in flight, and cancels any other job. */
export function scheduleDelete(b: MockBackend, id: string): void {
  const w = b.workspaces[id]
  if (w === undefined || b.jobs[id] === 'delete') return
  cancelJob(b, id)
  clearDetail(b, id, 'deleting')
  const failAt = b.failAction ?? undefined
  b.failAction = null
  schedule(b, id, deleteScript(b, id, w.state === 'deleting' ? null : w.state, failAt), 'delete')
}

/** Plays a held script (manual mode) to the end, or its first `n` events. */
export function playScript(b: MockBackend, id: string, n?: number): void {
  const script = b.scripts[id] ?? []
  const now = script.splice(0, n ?? script.length)
  for (const play of now) play()
}

function envelope(status: number, code: string, message: string, headers: Record<string, string> = {}, detail?: string) {
  return HttpResponse.json({ error: { code, message, ...(detail ? { detail } : {}) } }, { status, headers })
}

/** internal/secrets.Invalid's messages, by code: the server's prose, which the UI never shows. */
const INVALID_MESSAGE: Record<string, string> = {
  secret_name_invalid: "A secret's name is its environment variable name.",
  secret_value_empty: 'A secret needs a value.',
  secret_value_too_long: 'That value is too long.',
  secret_value_control_character: "A secret's value must be a single line with no control characters.",
  secret_reach_required: 'Say what someone could do with this secret.',
  secret_reach_too_long: 'The reach is too long.',
  secret_description_too_long: 'The description is too long.',
  secret_description_invalid: 'The description must be text.',
}

const refused = (r: SecretRefusal, name: string) =>
  envelope(400, r.code, r.code === 'secret_name_reserved' ? `${name} is reserved.` : INVALID_MESSAGE[r.code] ?? r.code, {}, r.detail)

/**
 * Reads a body the way internal/api's `decode` does: a JSON object, every
 * field of the right type, and no field it does not know.
 */
function strictBody(text: string, fields: Record<string, 'string' | 'boolean' | 'ints'>): Record<string, unknown> | null {
  let body: unknown
  try {
    body = JSON.parse(text)
  } catch {
    return null
  }
  if (body === null || typeof body !== 'object' || Array.isArray(body)) return null
  for (const [k, v] of Object.entries(body)) {
    const want = fields[k]
    if (want === undefined) return null
    if (want === 'ints' ? !(v === null || (Array.isArray(v) && v.every(Number.isInteger))) : typeof v !== want) return null
  }
  return body as Record<string, unknown>
}

const unauthenticated = () => envelope(401, 'unauthenticated', 'Sign in to continue.')
const busy = () => envelope(409, 'in_progress',
  'This repository already has a workspace, or this workspace is busy or not in a state that allows this.')
const appNotConfigured = () =>
  envelope(503, 'app_not_configured', 'No GitHub App is configured, so there is no repository list.')

/** The handlers, closed over one backend so a spec can reach in and change it. */
export function handlersFor(b: MockBackend): HttpHandler[] {
  // internal/api writeProvisionError: the detail names the configured cap.
  const atCapacity = () => envelope(409, 'at_capacity',
    'Drydock is at its concurrent-container cap. Stop a workspace to make room.', {}, `The cap is ${b.capacity}.`)
  const record = (request: Request) => {
    b.log.push({
      method: request.method,
      url: request.url,
      credentials: request.credentials,
      mode: request.mode,
      contentType: request.headers.get('Content-Type'),
    })
  }

  /** Records a secret write, raw body included, and returns the body for parsing. */
  const secretWrite = async (request: Request): Promise<string> => {
    record(request)
    const body = await request.text()
    b.secretBodies.push({ method: request.method, url: request.url, body })
    return body
  }
  const takeRefusal = () => {
    const r = b.refuseNextSecret
    if (r === null) return null
    b.refuseNextSecret = null
    return envelope(r.status, r.code, r.message, {}, r.detail)
  }
  const secretsNotConfigured = () => envelope(503, 'secrets_not_configured',
    'No secrets master key is configured, so secrets cannot be stored.',
    {}, 'Start drydock serve with --secrets-key; the installer creates the key.')

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

    http.get('/api/workspaces', ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      return HttpResponse.json(workspaceList(b), { headers: { 'Cache-Control': 'no-store' } })
    }),

    // POST /api/workspaces, checked in the order the server's one IMMEDIATE
    // transaction checks it: the body, the repository, the duplicate, the cap.
    http.post('/api/workspaces', async ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      if (!b.appConfigured) return appNotConfigured()
      let body: { repository_id?: unknown; branch?: unknown } | null
      try {
        body = (await request.json()) as typeof body
      } catch {
        body = null
      }
      const repoId = body?.repository_id
      const branch = body?.branch
      if (body === null || typeof body !== 'object' || !Number.isInteger(repoId)
        || (branch !== undefined && (typeof branch !== 'string' || branch === ''))) {
        return envelope(400, 'bad_request', 'Send a JSON body with a repository_id.')
      }
      const repo = b.repos.find((r) => r.id === repoId)
      if (repo === undefined || repo.removed) return envelope(404, 'not_found', 'No such repository.')
      const rows = Object.values(b.workspaces)
      // One repository, one workspace, until a delete finishes (design §5):
      // a row in any state — failed, stopped and deleting included — refuses.
      if (rows.some((w) => w.repository_id === repoId)) {
        return envelope(409, 'in_progress', 'That repository already has a workspace.')
      }
      if (rows.filter((w) => OCCUPYING.has(w.state)).length >= b.capacity) return atCapacity()
      const id = nextWorkspaceId(b)
      const failAt = b.failNext ?? undefined
      b.failNext = null
      schedule(b, id, cloneScript(b, repo.id, id, failAt, typeof branch === 'string' ? branch : repo.default_branch))
      return HttpResponse.json({ id }, { status: 202 })
    }),

    http.get('/api/workspaces/:id', ({ request, params }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      const w = b.workspaces[String(params.id)]
      if (w === undefined) return envelope(404, 'not_found', 'No such workspace.')
      return HttpResponse.json(workspaceDetail(b, w), { headers: { 'Cache-Control': 'no-store' } })
    }),

    http.post('/api/workspaces/:id/start', ({ request, params }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      const id = String(params.id)
      const w = b.workspaces[id]
      if (w === undefined) return envelope(404, 'not_found', 'No such workspace.')
      if (w.state !== 'stopped' && w.state !== 'failed') {
        return envelope(409, 'in_progress', 'The workspace is not stopped or failed.')
      }
      // A start takes a container slot, so the cap applies as to a create.
      if (Object.values(b.workspaces).filter((x) => OCCUPYING.has(x.state)).length >= b.capacity) return atCapacity()
      const failAt = b.failNext ?? undefined
      b.failNext = null
      schedule(b, id, startScript(b, id, failAt))
      return HttpResponse.json({}, { status: 202 })
    }),

    // Phase 6 (internal/api/workspace_routes.go, internal/provision
    // lifecycle.go): each answers 202 {} once its job is started.
    http.post('/api/workspaces/:id/stop', ({ request, params }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      const id = String(params.id)
      const w = b.workspaces[id]
      if (w === undefined) return envelope(404, 'not_found', 'There is no such workspace.')
      // Only a running workspace with nothing in flight stops: a build is
      // refused rather than cancelled (design §6).
      if (w.state !== 'running' || b.jobs[id] !== undefined) return busy()
      scheduleStop(b, id)
      return HttpResponse.json({}, { status: 202 })
    }),

    // Phase 5 (internal/api/supervisor_routes.go): start or restart the
    // session server, on a running workspace with nothing in flight.
    http.post('/api/workspaces/:id/supervisor', ({ request, params }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      const id = String(params.id)
      const w = b.workspaces[id]
      if (w === undefined) return envelope(404, 'not_found', 'There is no such workspace.')
      if (!b.supervisor) return envelope(501, 'not_implemented', 'Not implemented.')
      if (w.state !== 'running' || b.jobs[id] !== undefined) {
        return envelope(409, 'in_progress', 'The session server can be restarted only on a running workspace with nothing else in progress.')
      }
      schedule(b, id, supervisorScript(b, id), 'run')
      return HttpResponse.json({}, { status: 202 })
    }),

    // The session server's log: redacted lines, in memory on the server.
    http.get('/api/workspaces/:id/logs', ({ request, params }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      const id = String(params.id)
      const w = b.workspaces[id]
      if (w === undefined) return envelope(404, 'not_found', 'There is no such workspace.')
      if (!b.supervisor || !w.supervisor) return HttpResponse.json({ lines: [], truncated: false, held: false })
      const at = w.supervisor.at
      const env = mockEnvironmentId(id)
      const text = [
        '— Starting the session server.', 'Remote Control v2.1.289', 'Spawn mode: worktree',
        'Max concurrent sessions: 4', `Environment ID: ${env}`, '·✔︎· Connected · repo',
        '    Capacity: 1/4 · New sessions will be created in an isolated worktree',
        `Continue coding in the Claude mobile app or https://claude.ai/code?environment=${env}`,
        'token in output: [redacted]',
      ]
      return HttpResponse.json({ lines: text.map((t, i) => ({ n: i + 1, at, text: t })), truncated: false, held: true })
    }),

    // Rebuild is start's run with --remove-existing-container: from step 3,
    // or step 2 for a failed clone — the same events as a start.
    http.post('/api/workspaces/:id/rebuild', ({ request, params }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      if (!b.appConfigured) {
        return envelope(503, 'app_not_configured', 'No GitHub App is configured, so Drydock cannot clone, start or rebuild a workspace.')
      }
      const id = String(params.id)
      const w = b.workspaces[id]
      if (w === undefined) return envelope(404, 'not_found', 'There is no such workspace.')
      if (b.jobs[id] !== undefined || !['running', 'stopped', 'failed'].includes(w.state)) return busy()
      // A running workspace holds its slot already; a stopped or failed one takes one.
      if (w.state !== 'running' && Object.values(b.workspaces).filter((x) => OCCUPYING.has(x.state)).length >= b.capacity) {
        return atCapacity()
      }
      const failAt = b.failNext ?? undefined
      b.failNext = null
      schedule(b, id, startScript(b, id, failAt))
      return HttpResponse.json({}, { status: 202 })
    }),

    // DELETE ?confirm=<full_name>, compared exactly — no trimming, no case
    // folding. A delete in flight is joined, one that stuck is resumed, and
    // any other job is cancelled first.
    http.delete('/api/workspaces/:id', ({ request, params }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      const id = String(params.id)
      const w = b.workspaces[id]
      if (w === undefined) return envelope(404, 'not_found', 'There is no such workspace.')
      const fullName = b.repos.find((r) => r.id === w.repository_id)?.full_name ?? ''
      const confirm = new URL(request.url).searchParams.get('confirm') ?? ''
      if (fullName === '' || confirm !== fullName) {
        return envelope(400, 'confirm_mismatch', "To delete this workspace, confirm with the repository's full name, exactly.")
      }
      scheduleDelete(b, id)
      return HttpResponse.json({}, { status: 202 })
    }),

    // The Claude identity (design §7.3, internal/api/claude_routes.go). The
    // login half is null: the handshake is not built.
    http.get('/api/auth/claude', ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      return HttpResponse.json({ identity: b.identity, login: null }, { headers: { 'Cache-Control': 'no-store' } })
    }),

    // A check reads the same stored state again — or, in a spec, whatever
    // the spec set — and announces it, as the watch does after a 202.
    http.post('/api/auth/claude/check', ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      b.identityChecks++
      setTimeout(() => setIdentity(b, { ...b.identity, check_error: null, last_checked_at: new Date().toISOString() }), 0)
      return HttpResponse.json({}, { status: 202 })
    }),

    // /api/secrets (design §10, internal/api/secret_routes.go). 200 and 204,
    // not 202. No route returns a value: secretMeta has nowhere to put one.
    http.get('/api/secrets', ({ request }) => {
      record(request)
      if (!b.signedIn) return unauthenticated()
      if (!b.secretsKey) return secretsNotConfigured()
      const secrets = Object.values(b.secrets).sort((x, y) => x.name.localeCompare(y.name)).map((s) => secretMeta(b, s))
      return HttpResponse.json({ secrets, undeliverable: b.undeliverable }, { headers: { 'Cache-Control': 'no-store' } })
    }),

    http.put('/api/secrets/:name/grants', async ({ request, params }) => {
      const body = await secretWrite(request)
      if (!b.signedIn) return unauthenticated()
      if (!b.secretsKey) return secretsNotConfigured()
      const forced = takeRefusal()
      if (forced) return forced
      const parsed = strictBody(body, { repository_ids: 'ints', all_repos: 'boolean' })
      if (parsed === null) return envelope(400, 'bad_request', 'Send a JSON object with the documented fields.')
      const s = b.secrets[String(params.name)]
      if (s === undefined) return envelope(404, 'not_found', 'There is no secret by that name.')
      const ids = [...new Set((parsed.repository_ids as number[] | null | undefined) ?? [])]
      const unknown = ids.find((id) => !b.repos.some((r) => r.id === id))
      if (unknown !== undefined) {
        return envelope(400, 'unknown_repository', 'That repository is not in the catalog.', {},
          `No repository has id ${unknown}. Refresh the repository list and try again.`)
      }
      s.grants = ids
      s.all_repos = parsed.all_repos === true
      const meta = secretMeta(b, s)
      emit(b, 'secret.grants', {
        message: s.all_repos ? `Granted the secret ${s.name} to every repository.` : `Granted the secret ${s.name} to ${ids.length} repositories.`,
        data: { secret: meta },
      })
      return HttpResponse.json({ secret: meta })
    }),

    http.put('/api/secrets/:name', async ({ request, params }) => {
      const body = await secretWrite(request)
      if (!b.signedIn) return unauthenticated()
      if (!b.secretsKey) return secretsNotConfigured()
      const forced = takeRefusal()
      if (forced) return forced
      const parsed = strictBody(body, { value: 'string', reach: 'string', description: 'string' })
      if (parsed === null) return envelope(400, 'bad_request', 'Send a JSON object with the documented fields.')
      const name = String(params.name)
      // An absent value keeps the stored one; "" is a value, and refused
      // (internal/api optionalValue). A null fails strictBody's type check
      // above, as the server's decoder refuses it.
      const kept = !('value' in parsed)
      const reach = String(parsed.reach ?? '')
      const description = String(parsed.description ?? '')
      // internal/secrets Put's order: name, value, reach, description.
      for (const r of [checkName(name), kept ? null : checkValue(String(parsed.value)), checkReach(reach), checkDescription(description)]) {
        if (r !== null) return refused(r, name)
      }
      const now = new Date().toISOString()
      const cur = b.secrets[name]
      if (kept && cur === undefined) {
        return envelope(400, 'secret_value_required', 'A new secret needs a value.', {},
          'There is no secret by this name, so there is no stored value to keep.')
      }
      const value = kept ? cur!.value : String(parsed.value)
      const res: PutSecretResult = {
        created: cur === undefined, rotated: false,
        stale: { new_commands: [], needs_supervisor_restart: [] },
        secret: undefined as unknown as SecretMeta,
      }
      if (cur === undefined) {
        b.secrets[name] = {
          name, value, reach, description, all_repos: false, grants: [], created_at: now, rotated_at: null,
          last_access_at: null, accessed_by: [],
        }
      } else {
        res.rotated = cur.value !== value
        b.secrets[name] = { ...cur, value, reach, description, rotated_at: res.rotated ? now : cur.rotated_at }
      }
      const s = b.secrets[name]!
      if (res.rotated) res.stale = staleFor(b, s)
      res.secret = secretMeta(b, s)
      if (res.created) {
        emit(b, 'secret.created', { message: `Stored the secret ${name}. It is granted to nothing yet.`, data: { secret: res.secret } })
      } else if (res.rotated) {
        const n = res.stale.new_commands.length + res.stale.needs_supervisor_restart.length
        emit(b, 'secret.rotated', { message: `Rotated the secret ${name}; ${n} running workspaces hold it.`, data: { secret: res.secret, stale: res.stale } })
      } else {
        emit(b, 'secret.updated', { message: `Updated the reach and description of the secret ${name}; its value is unchanged.`, data: { secret: res.secret } })
      }
      // Storing a value again is how a row the key cannot open is repaired.
      if (!kept) repairDelivery(b, name, 'does_not_open')
      return HttpResponse.json(res)
    }),

    http.delete('/api/secrets/:name', async ({ request, params }) => {
      await secretWrite(request)
      if (!b.signedIn) return unauthenticated()
      if (!b.secretsKey) return secretsNotConfigured()
      const forced = takeRefusal()
      if (forced) return forced
      const name = String(params.name)
      if (b.secrets[name] === undefined) return envelope(404, 'not_found', 'There is no secret by that name.')
      delete b.secrets[name]
      emit(b, 'secret.deleted', { message: `Deleted the secret ${name}.`, data: { name } })
      repairDelivery(b, name)
      return new HttpResponse(null, { status: 204 })
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
