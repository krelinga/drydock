// The API's shapes, as the Go handlers write them.
//
// Frontend §4.5 says these are to be *generated* from the Go structs, so the
// contract has one source. No generator exists yet, so they are written by
// hand and mirrored field for field from internal/api/session_routes.go
// (deviceJSON), internal/api/problem.go (Error), internal/catalog/view.go
// (View), internal/events/events.go (Event), internal/workspace/view.go and
// internal/api/workspace_routes.go (View, WorkspaceList) and internal/secrets/store.go
// (Meta, PutResult), with each event kind's `data`
// read off the Emit call that writes it (see stores/reducer.ts). This file is
// the one to replace with generated output rather than extend much further.

/** One signed-in device, from `GET /api/auth/session`. */
export interface Device {
  /** SHA-256 of the cookie: safe to display, useless as a cookie. */
  id: string
  label: string
  created_ip: string
  created_at: string
  last_seen_at: string
  expires_at: string
  /** Frontend §4.5 #6: so revoking can warn that this one is you. */
  is_current: boolean
}

/** `GET /api/auth/session`. */
export interface SessionInfo {
  current: string
  devices: Device[]
}

/** The one error shape every route returns (frontend §4.5 #7). */
export interface ErrorEnvelope {
  error: {
    code: string
    message: string
    /** Context for the code: which rule, which character. Shown as text, never parsed. */
    detail?: string
  }
}

/**
 * The stable codes from internal/api/problem.go, plus two the client itself
 * produces: `network` when no response arrived at all, and `unparseable` when
 * one did but it was not the envelope (an HTML page where JSON was expected is
 * exactly the bug frontend §3 rule 3 exists to prevent).
 */
export type ErrorCode =
  | 'unauthenticated'
  | 'forbidden_origin'
  | 'forbidden_host'
  | 'not_found'
  | 'method_not_allowed'
  | 'not_implemented'
  | 'in_progress'
  | 'at_capacity'
  | 'confirm_mismatch'
  | 'bad_request'
  | 'bad_password'
  | 'locked_out'
  | 'not_configured'
  | 'app_not_configured'
  | 'secrets_not_configured'
  | 'secret_name_invalid'
  | 'secret_name_reserved'
  | 'secret_value_required'
  | 'secret_value_empty'
  | 'secret_value_control_character'
  | 'secret_value_too_long'
  | 'secret_reach_required'
  | 'secret_reach_too_long'
  | 'secret_description_too_long'
  | 'secret_description_invalid'
  | 'unknown_repository'
  | 'internal'
  | 'network'
  | 'unparseable'

/** `internal/workspace/state.go`. */
export type WorkspaceState = 'pending' | 'cloning' | 'building' | 'running' | 'stopped' | 'failed' | 'deleting'

export const WORKSPACE_STATES: readonly WorkspaceState[] = [
  'pending', 'cloning', 'building', 'running', 'stopped', 'failed', 'deleting',
]

/**
 * Design §6's eight steps, in the order a clone runs them. A step a run has
 * not reached is simply absent from a workspace's `steps`.
 */
export const WORKSPACE_STEPS = [
  'allocate', 'clone', 'resolve_config', 'credential_volume', 'broker_socket', 'up', 'verify', 'session_server',
] as const

export type StepStatus = 'started' | 'done' | 'failed'

/** A step's latest status, as `GET /api/workspaces` reports it. */
export interface StepView {
  status: StepStatus
  /** A `workspace.Public` sentence; never a raw error. */
  detail?: string
  at: string
}

/** One workspace, from `GET /api/workspaces` and `GET /api/workspaces/:id`. */
export interface WorkspaceView {
  id: string
  repository_id: number
  full_name: string
  branch: string
  state: WorkspaceState
  state_detail: string | null
  container_id: string | null
  created_at: string
  /** Keyed by step name; each step's latest status across runs. */
  steps: Record<string, StepView>
  /**
   * The latest `workspace.action` event — a stop's or a delete's newest
   * sub-step — or null when there has been none (frontend §4.5 #15). Absent
   * from a server older than it.
   */
  last_action?: ActionView | null
}

/** One `workspace.action` event as the view reports it (internal/workspace ActionOutcome). */
export interface ActionView {
  action: string
  step: string
  status: StepStatus
  detail?: string
  at: string
}

/**
 * The concurrent-container cap and how many workspaces count against it
 * (frontend §4.5 #17): occupied is the list's own rows counted by
 * `workspace.Occupying`. `cap` is null when there is none.
 */
export interface CapacityView {
  cap: number | null
  occupied: number
}

/** `GET /api/workspaces`: every workspace with a row, newest first, `deleting` included. */
export interface WorkspaceList {
  workspaces: WorkspaceView[]
  /** Absent from a server older than §4.5 #17. */
  capacity?: CapacityView
}

/** `GET /api/workspaces/:id`: the view plus its latest 50 events, newest first. */
export interface WorkspaceDetail extends WorkspaceView {
  events: StreamEvent[]
}

/** One installation of the GitHub App, from `GET /api/repos`. */
export interface InstallationView {
  id: number
  account: string
  /** Where the operator adds or removes repositories (§9.4, frontend §4.5 #8). */
  settings_url: string
}

/** One repository, joined with its newest workspace, from `GET /api/repos`. */
export interface RepoView {
  id: number
  installation_id: number
  full_name: string
  default_branch: string
  private: boolean
  archived: boolean
  /** Null is *unknown* — never render it as "no dev container". */
  has_devcontainer: boolean | null
  pushed_at: string | null
  /** The installation no longer covers it, but a workspace still holds it. */
  removed: boolean
  workspace: { id: string; state: WorkspaceState } | null
}

/** `GET /api/repos`. */
export interface CatalogView {
  /** Null before the first refresh: "loading", not "empty". */
  refreshed_at: string | null
  /**
   * The most recent refresh's failure, null when it succeeded — how a page
   * loaded after the `repo.refresh_failed` event still learns of it. Its
   * message is the server's sentence, shown as text and never parsed.
   */
  last_refresh_error: { at: string; message: string } | null
  installations: InstallationView[]
  repos: RepoView[]
}

/**
 * One unnamed event on `GET /api/events` (internal/events.Event). `message`
 * is for a human and nothing may parse it; `data` is what the reducer applies.
 */
export interface StreamEvent {
  id: number
  /** Absent for events about the whole system. */
  workspace_id?: string
  level: 'info' | 'warn' | 'error'
  kind: string
  message: string
  data?: Record<string, unknown>
  at: string
}

/** One repository a secret may reach (internal/secrets.Grant). */
export interface SecretGrant {
  repository_id: number
  full_name: string
}

/**
 * Everything about a secret except its value (internal/secrets.Meta). There
 * is no field a value could go in, here or in the Go type: no route returns
 * one (design §13.5, frontend §2.5).
 */
export interface SecretMeta {
  name: string
  /** The answer to "what can someone do with this?" (design §10.4). */
  reach: string
  description: string
  /** Every repository, including ones added to the installation later. */
  all_repos: boolean
  grants: SecretGrant[]
  created_at: string | null
  /** Null until the value first changes. */
  rotated_at: string | null
  last_access_at: string | null
  /** Workspace ids that have fetched it, most recent first. */
  accessed_by: string[]
}

/**
 * One stored secret that cannot be delivered (internal/secrets.UndeliverableSecret).
 * `does_not_open`: the master key cannot decrypt it — storing the value again
 * repairs it. `breaks_write_rules`: only a database edit makes one; deleting it
 * is the repair.
 */
export interface UndeliverableSecret {
  name: string
  reason: 'does_not_open' | 'breaks_write_rules' | (string & {})
}

/**
 * Why stored secrets cannot be delivered (internal/secrets.Undeliverable):
 * while it holds, every workspace's commands fail (design §10.3). Names and
 * reasons only — never a value.
 */
export interface Undeliverable {
  since: string
  secrets: UndeliverableSecret[]
}

/** `GET /api/secrets`. */
export interface SecretList {
  secrets: SecretMeta[]
  /** Null when every stored secret can be delivered (frontend §4.5 #12). */
  undeliverable?: Undeliverable | null
}

/** A running workspace a rotation reached (internal/secrets.StaleWorkspace). */
export interface StaleWorkspace {
  workspace_id: string
  repository_id: number
  full_name: string
}

/**
 * Which running workspaces hold a rotated value, by what it costs them
 * (design §10.3, frontend §4.5 #5). Both lists are always present.
 */
export interface Stale {
  new_commands: StaleWorkspace[]
  needs_supervisor_restart: StaleWorkspace[]
}

/** `PUT /api/secrets/:name`'s 200 body (internal/secrets.PutResult). */
export interface PutSecretResult {
  secret: SecretMeta
  created: boolean
  rotated: boolean
  stale: Stale
}

/** `claude_identity.state` (design §4, §7.3): the five-way verdict, stored once by the watch. */
export type IdentityState = 'ok' | 'expiring' | 'expired' | 'blanked' | 'absent'

export const IDENTITY_STATES: readonly IdentityState[] = ['ok', 'expiring', 'expired', 'blanked', 'absent']

/** The last identity check's failure (internal/identity.CheckError). Drydock's own sentence. */
export interface IdentityCheckError {
  at: string
  /** docker | image | credentials | auth_status | disagree | foreign_volume | unknown */
  problem: string
  message: string
}

/**
 * The stored identity (internal/identity.View), from `GET /api/auth/claude`
 * and the `auth.identity` event. `state` is null until a check has ever
 * succeeded — "not yet known", never one of the five.
 */
export interface IdentityView {
  state: IdentityState | null
  /** Only beside a login (ok, expiring, expired). */
  account_email: string | null
  expires_at: string | null
  /** When Drydock first saw this login. */
  logged_in_at: string | null
  last_checked_at: string | null
  /** The shared credential volume's name. */
  volume: string
  /** Null when the last check succeeded. The stored state stands either way. */
  check_error: IdentityCheckError | null
}

/** `GET /api/auth/claude`. `login` is the in-flight handshake: always null until it is built. */
export interface ClaudeIdentityBody {
  identity: IdentityView
  login: null
}
