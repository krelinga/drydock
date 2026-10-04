// The API's shapes, as the Go handlers write them.
//
// Frontend §4.5 says these are to be *generated* from the Go structs, so the
// contract has one source. No generator exists yet, and Phase 1 has exactly two
// shapes, so they are written by hand here and mirrored field for field from
// internal/api/session_routes.go (deviceJSON) and internal/api/problem.go
// (Error). When Phase 2 adds the event stream's dozen kinds, this file is the
// one to replace with generated output rather than extend.

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
  | 'bad_request'
  | 'bad_password'
  | 'locked_out'
  | 'not_configured'
  | 'app_not_configured'
  | 'internal'
  | 'network'
  | 'unparseable'
