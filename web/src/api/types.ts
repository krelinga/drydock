// The API's shapes, as the Go handlers write them.
//
// Frontend §4.5 says these are to be *generated* from the Go structs, so the
// contract has one source. No generator exists yet, so they are written by
// hand and mirrored field for field from internal/api/session_routes.go
// (deviceJSON), internal/api/problem.go (Error), internal/catalog/view.go
// (View) and internal/events/events.go (Event), with each event kind's `data`
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
  | 'bad_request'
  | 'bad_password'
  | 'locked_out'
  | 'not_configured'
  | 'app_not_configured'
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
}

/** `GET /api/workspaces`: every workspace with a row, newest first, `deleting` included. */
export interface WorkspaceList {
  workspaces: WorkspaceView[]
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
