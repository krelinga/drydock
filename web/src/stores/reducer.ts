// The reducer (frontend §4.1, §10): the one function that writes entity state.
//
// It is pure — (entities, action) → entities, never mutating its input — so
// every case it handles is a plain Vitest test over a recorded event sequence
// (reducer.spec.ts). Five kinds of input reach it:
//
//   event       an unnamed frame off GET /api/events
//   resync      the one named frame: replay could not close the gap
//   snapshot    a GET /api/repos body, with the stream position it was asked at
//   workspaces  a GET /api/workspaces body, likewise — and the one input that
//               writes the cap (its `capacity`); the occupied count is never
//               stored, it is counted from the entities (lib/capacity.ts)
//   workspace   a GET /api/workspaces/:id body — one workspace and its recent
//               events — likewise
//
// Of the three snapshots only `workspaces` is the authority on which
// workspaces exist: it lists every row, where the catalog joins only each
// repository's newest (and not a `deleting` one) and the detail names one. So
// it alone drops a workspace it does not mention.
//
// Ordering is by event id, not arrival. Every field that events write carries
// the id of the event that last wrote it, and an event at or below that id is
// a no-op. That one rule is what makes a replayed gap idempotent (the browser
// re-sends Last-Event-ID; the server may overlap what we already applied) and
// what keeps a late, out-of-order event from rolling a state backwards. Ids are
// assigned by the server under the same lock that publishes them
// (internal/events), so a higher id is always the later fact.
//
// Event data shapes, from the Emit calls that write them:
//
//   workspace.state    {state, from, detail?}                  workspace.Move
//                      {state: stopped, from: building, detail, approval}
//                                           a run stopped for a host-access
//                                           approval (design §6); every other
//                                           workspace.state carries none, which
//                                           is what takes the request away
//   config.approved    {repository_id, hash}  the approval recorded; settles
//                                           the approve button, changes nothing
//                      {state: running, from, container_id}    Move to running
//                      {state, repository_id, branch}          workspace.Create
//                      {state, adopted, repository_id, branch, detail?}  Adopt
//   workspace.step     {step, status: started|done|failed|needs_approval, detail?}
//   workspace.action   {action: stop|delete, step, status: started|done|failed, detail?}
//                                           internal/provision subSteps (Phase 6)
//                      {state: deleting, from: deleting, detail}  Annotate: a stuck
//                                           delete, written as a workspace.state
//                      {state: running, from: running, detail}  Annotate: a failed
//                                           stop (frontend §4.5 #15)
//                      {state, from: state}  ClearDetail: a retry clearing either
//                                           as it starts (§4.5 #15, #16)
//   workspace.gone     {}
//   supervisor.state   {state, from, reason, detail, restart_count}
//                                           internal/supervisor: the session
//                                           server's process state (design §8)
//   session.status     {environment_id, url, capacity_used, capacity_total, sessions}
//                                           what the server's output announced
//   workspace.adopted  {container_id}       a known row matched to a container
//   repo.refreshed     {count, added, removed}
//   repo.refresh_failed {}                  the reason is in `message`
//   secret.created     {secret}             internal/secrets Put, Meta
//   secret.rotated     {secret, stale}      the value changed
//   secret.grants      {secret}             SetGrants
//   secret.deleted     {name}
//   secret.updated     {secret}             reach or description only: the
//                                           value is unchanged, never a rotation
//   secret.undeliverable  {undeliverable: {since, secrets: [{name, reason}]}}
//                                           level error; sent when delivery
//                                           breaks or the broken set changes
//   secret.deliverable    {}                the condition cleared
//   auth.identity      {identity}           internal/identity: the stored verdict
//                                           changed, or a failing check recovered
//   auth.identity_check_failed {check_error: {at, problem, message}}
//                                           the stored state stands
//   auth.identity_checked {identity}        a check someone asked for found
//                                           nothing to announce: the same
//                                           view, last_checked_at moved. It
//                                           is what settles "Check now" (§4.2)
//   auth.login         {login}              internal/login: the handshake's
//                                           every phase change, the whole view
//
// The login handshake (design §7.2) is one fleet-wide value beside the
// identity, written by the same snapshot's `login` and by auth.login, and
// versioned on its own. It holds no code: there is no field for one.
//
// The Claude identity has its own snapshot, `identity` — a GET
// /api/auth/claude body — and is written by it and the three auth.identity*
// events, versioned by one id like every field. It is one fleet-wide value,
// not a per-workspace one: the banner and the card overlay read the same
// field, which is the whole of frontend §6.6.
//
// Resources — memory and disk — have a seventh, `resources`: the stream's
// named frame, one per sampling round, with no event id (it is a measurement
// and is never persisted). They and the host's disk are also carried by the
// workspace list and detail, and every copy is versioned by the server's round
// time rather than an event id (stores/resources.ts). They live beside the
// workspaces, not in them: a measurement changes nothing about a workspace.
//
// Secrets have a sixth input, `secrets` — a GET /api/secrets body — and are
// written by it and by the secret.* events, and by nothing else. The same
// two inputs write the fleet fault: the body's `undeliverable` (null when
// delivery works) and the two delivery events, versioned by one id like any
// field, so a reload shows a standing fault and a repair clears it. PUT
// /api/secrets/:name answers 200 with the secret's metadata, and that body is
// deliberately *not* an input: it carries no event id, so it could not be
// ordered against an event from another device that beat it here, and
// applying it would be the second write path §2.1 rules out. The event that
// the same write emitted arrives on the stream and carries the same metadata.
// (`stale` in secret.rotated is the operation's result, not the secret's
// state; it is shown by the screen that asked — views/secrets — and not kept.)
//
// Every other kind (token.*, container.unclaimed, system.reconcile, and
// whatever a later phase adds) advances the stream position and changes no
// workspace or repository: if it names a workspace it joins that workspace's
// feed, and that is all. An unknown kind must never be an error — the server
// names nothing precisely so a kind the client does not know reaches this
// branch rather than vanishing (events_routes.go).

import {
  IDENTITY_STATES, LOGIN_PHASES, WORKSPACE_STATES, WORKSPACE_STEPS, type CatalogView, type ClaudeIdentityBody,
  type IdentityCheckError, type IdentityState, type InstallationView, type LoginPhase, type RepoView, type SecretList,
  type SecretMeta, type StepStatus, type StreamEvent, type ApprovalView, type WorkspaceDetail, type WorkspaceList, type WorkspaceState,
  type WorkspaceView, SUPERVISOR_STATES, type SupervisorState, type HostDiskView, type ResourcesView,
  type HostHeader, type PortList, type PortView, type DiscoveryState,
} from '../api/types'
import { frameParts, mergeHostDisk, mergeResources, type HostDisk, type Resources } from './resources'

/**
 * The shared Claude login as the watch last stored it (design §7.3). Named
 * fields only: nothing about the credential is in the API, and nothing here
 * could hold it.
 */
export interface ClaudeIdentity {
  /** Null: no check has ever succeeded — not yet known, never "absent". */
  state: IdentityState | null
  accountEmail: string | null
  /** The access token's expiry: hours, renewed by every refresh. Never a countdown. */
  expiresAt: string | null
  /** The login's (the refresh token's) expiry; what `expiring` counts down to. */
  loginExpiresAt: string | null
  loggedInAt: string | null
  lastCheckedAt: string | null
  volume: string
  /** The last check's failure, null when it succeeded. */
  checkError: IdentityCheckError | null
}

/**
 * The session server's process state (design §8), from `supervisor.state`
 * events and the views' `supervisor`. Its own state machine, beside the
 * workspace's: the card joins the two (lib/workspaceCard.ts).
 */
export interface Supervisor {
  state: SupervisorState
  /** A code (internal/supervisor Reason): the card's sentence is chosen by it, never by `detail`. */
  reason: string | null
  /** Drydock's sentence for the reason. Shown, never parsed. */
  detail: string | null
  restartCount: number
  /** When it entered this state: the waiting card's elapsed time. */
  since: string
}

/** What the session server announced (`session.status`). */
export interface SessionStatus {
  environmentId: string | null
  /** Built here from the id, never taken from the wire: an href is a capability to send the user somewhere. */
  url: string | null
  capacityUsed: number | null
  capacityTotal: number | null
  sessions: number
}

/** The login handshake (design §7.2, frontend §6.2). No field can hold a code. */
export interface ClaudeLogin {
  id: string
  phase: LoginPhase
  url: string | null
  deadline: string | null
  startedAt: string
  endedAt: string | null
  attempts: number
  problem: string | null
  message: string
}

export interface StepInfo {
  name: string
  status: StepStatus
  detail: string | null
}

/** One step's latest status, versioned on its own. */
export interface StepRecord {
  status: StepStatus
  detail: string | null
  /** When the server says it happened. */
  at: string
  /** The id of the event (or snapshot position) that wrote it. */
  eventId: number
}

/** How many events a workspace's feed keeps: what GET /api/workspaces/:id returns. */
export const FEED_LIMIT = 50

export interface Workspace {
  id: string
  /** Null when only an event without one has been seen (a stub). */
  repositoryId: number | null
  /** From a snapshot: the workspace list's own, or the catalog row's. */
  fullName: string | null
  branch: string | null
  /** Null for a stub: an event named this workspace before anything said its state. */
  state: WorkspaceState | null
  /** `state_detail`: the server's sentence about the state. Shown, never parsed. */
  detail: string | null
  /** The most recent step event: what the card's progress line names. */
  step: StepInfo | null
  /** Each step's latest status, by name: the detail view's timeline. */
  steps: Record<string, StepRecord>
  containerId: string | null
  createdAt: string | null
  adopted: boolean
  /** The id of the event (or snapshot position) that last wrote `state`. */
  stateAt: number
  /**
   * The id of the event (or snapshot position) that last wrote
   * `containerId`. Versioned apart from `state`: the move to running and
   * `workspace.adopted` both carry the id, and the latter changes no state.
   */
  containerAt: number
  /** The id of the event that last wrote `step`. */
  stepAt: number
  /**
   * The latest stop or delete, sub-step by sub-step (`workspace.action`).
   * Whether it is still going is a question for `liveAction`, not this field:
   * a finished run is kept so a stuck delete can still name its sub-step.
   */
  action: ActionRun | null
  /**
   * The latest stop or delete sub-step, from `workspace.action` events or a
   * snapshot's `last_action`, whichever is newer. Unlike `action` it says
   * nothing about whether a run is in progress; it is what lets a card loaded
   * from the list alone name the sub-step a failed stop stopped at
   * (`stopFailed`), since the list carries no action events.
   */
  lastAction: LastAction | null
  /**
   * The id of the newest `workspace.state` *event* seen for this workspace,
   * live or in a detail body — never a snapshot position, unlike `stateAt`.
   * Every stop and delete ends in a state event (the move to stopped, a stuck
   * delete's annotation) or in `workspace.gone`, so an action run newer than
   * this is one still in progress, or one that failed and left the state
   * where it was.
   */
  stateEventId: number
  /** The session server's state, or null when none was ever started. */
  supervisor: Supervisor | null
  /**
   * Whether anything has said what the supervisor is: a supervisor.state
   * event, or a view carrying the `supervisor` field. A server older than
   * Phase 5 never sends it, and its running card must not claim "no session"
   * on the strength of a field it does not have.
   */
  supervisorKnown: boolean
  /** The id of the event (or snapshot position) that last wrote `supervisor`. */
  supervisorAt: number
  /** What the server last announced, or null. */
  session: SessionStatus | null
  /** The id of the event (or snapshot position) that last wrote `session`. */
  sessionAt: number
  /**
   * The host-access request a stopped workspace waits on (design §6), or
   * null. Written with `state` and versioned with it: the workspace.state
   * event that stopped the run carries it, every other one carries none.
   */
  approval: ApprovalView | null
}

/** The latest action sub-step, versioned like any field. */
export interface LastAction {
  /** `stop` or `delete`. */
  name: string
  step: string
  status: StepStatus
  detail: string | null
  /** The id of the event (or snapshot position) that wrote it. */
  at: number
}

/** One stop or delete, as its `workspace.action` events tell it. */
export interface ActionRun {
  /** `stop` or `delete` (internal/provision ActStop, ActDelete). */
  name: string
  /** Each sub-step's latest status in this run, by name. */
  steps: Record<string, StepRecord>
  /** The newest sub-step event: what the card's line names. */
  last: StepInfo
  /** The id of the event that began this run; anything older is an earlier run's. */
  startId: number
  /** The id of the newest event in this run. */
  eventId: number
}

export interface Repo {
  id: number
  installationId: number
  fullName: string
  defaultBranch: string
  private: boolean
  archived: boolean
  hasDevcontainer: boolean | null
  pushedAt: string | null
  removed: boolean
}

export interface RefreshOutcome {
  ok: boolean
  at: string
  count: number | null
  added: number | null
  removed: number | null
  /** The event's human sentence. Shown, never parsed. */
  message: string
  /** The event id, so an older outcome cannot replace a newer one. */
  eventId: number
}

/**
 * One secret's metadata (design §10.1). There is no value field, and nothing
 * that writes this type could fill one: neither the events nor the list carry
 * a value (frontend §2.5).
 */
export interface Secret {
  name: string
  reach: string
  description: string
  allRepos: boolean
  grants: Array<{ repositoryId: number; fullName: string }>
  createdAt: string | null
  rotatedAt: string | null
  lastAccessAt: string | null
  /** Workspace ids, most recent first. */
  accessedBy: string[]
  /** The id of the event (or snapshot position) that last wrote it. */
  at: number
}

/**
 * Stored secrets cannot be delivered: every workspace's commands fail until
 * it is fixed. From GET /api/secrets' `undeliverable` or `secret.undeliverable`,
 * whichever is newer; `secret.deliverable` or a snapshot reporting null clears it.
 */
export interface SecretFault {
  /** When the server first found it. */
  since: string | null
  /** The secrets that fail, by name, and why. Never a value: there is none to give. */
  secrets: Array<{ name: string; reason: string }>
}

/**
 * One live forwarded port (port forwarding §5): a permission to reach a
 * container port, off until enabled. From GET /api/workspaces/:id/ports and
 * the port.* events, which carry the whole row; nothing else writes it.
 */
export interface Port {
  id: string
  workspaceId: string
  containerPort: number
  slug: string
  host: string | null
  url: string | null
  label: string | null
  hostHeader: HostHeader
  enabled: boolean
  hidden: boolean
  declared: boolean
  observed: boolean
  manual: boolean
  /** What discovery last saw it bound to; null when it never has (PF §8.2). */
  bindAddr: string | null
  /** Bound to loopback: listed, but only the container itself can reach it. */
  loopback: boolean
  /** Discovery's verdict: listening now, gone (seen before, not now), or null for never seen. */
  observedState: 'listening' | 'gone' | null
  /** When discovery last saw it listening; null when it never has. */
  lastSeenAt: string | null
  /** The id of the event (or snapshot position) that last wrote it. */
  at: number
}

/**
 * A workspace's port discovery as the server last reported it (port
 * forwarding §11): by `port.scanned` and `port.discovery` (`data.discovery`)
 * and by the port list (`discovery`), versioned like any entity.
 */
export interface PortDiscovery {
  state: DiscoveryState
  /** The id of the event (or snapshot position) that last wrote it. */
  at: number
}

export interface Entities {
  /** The highest event id applied. What the next snapshot is "as of". */
  lastEventId: number
  /**
   * The id of a `resync` no snapshot has yet caught up with, or null. While it
   * is set the entities may be wrong: render them, but label them. Only a
   * catalog or workspace-list snapshot taken at or after this id clears it —
   * one already in flight when the resync arrived predates the gap and does
   * not, and a single workspace's detail says nothing about the rest.
   */
  staleSince: number | null
  workspaces: Record<string, Workspace>
  /**
   * Deleted workspaces, by the id of their `workspace.gone`. Ids are ULIDs and
   * never reused, so a late event or an older snapshot naming one is ignored
   * rather than resurrecting it.
   */
  gone: Record<string, number>
  /**
   * Each workspace's recent events, newest first, at most FEED_LIMIT. Fed by
   * every event naming a workspace — token.issued included — and by the
   * detail snapshot, merged by id so the two never double an entry.
   */
  feeds: Record<string, StreamEvent[]>
  repos: Record<number, Repo>
  /** The server's order (newest push first); `repos` is keyed, this is the list. */
  repoOrder: number[]
  installations: Record<number, InstallationView>
  /** Null until a snapshot has been applied: "not loaded", distinct from "empty". */
  catalogRefreshedAt: string | null
  catalogRefreshError: { at: string; message: string } | null
  catalogLoaded: boolean
  lastRefresh: RefreshOutcome | null
  /** By name. Only GET /api/secrets and secret.* events write it. */
  secrets: Record<string, Secret>
  /**
   * Deleted secrets, by the id of their `secret.deleted`. Unlike a workspace
   * id a name can come back, so this is a version, not a tombstone: a
   * `secret.created` after it recreates the secret, and a snapshot taken
   * before it cannot.
   */
  secretsDeleted: Record<string, number>
  /** Null until a GET /api/secrets has been applied: "not loaded", distinct from "none". */
  secretsLoaded: boolean
  /** Null while delivery works, or before anything has said otherwise. */
  secretFault: SecretFault | null
  /** The event id (or snapshot position) that last wrote `secretFault`, set or cleared. */
  secretFaultAt: number
  /**
   * The concurrent-container cap, from the last GET /api/workspaces; null
   * until one carrying it has been applied, or when the server has none. It
   * is configuration, so no event changes it — a restart that changed it
   * reopens the stream, and the reopen refetches the list.
   */
  cap: number | null
  /** Null until GET /api/auth/claude or an auth.identity event has been applied. */
  identity: ClaudeIdentity | null
  /** The event id (or snapshot position) that last wrote `identity`. */
  identityAt: number
  /** The login handshake in progress or just ended; null when there is none. */
  login: ClaudeLogin | null
  /** The event id (or snapshot position) that last wrote `login`, set or cleared. */
  loginAt: number
  /** Each workspace's latest measurements, by id; absent until measured. */
  resources: Record<string, Resources>
  /** The workspace filesystem against the disk limit; null until measured. */
  hostDisk: HostDisk | null
  /** Live forwarded ports, by row id. Only the port list and port.* events write it. */
  ports: Record<string, Port>
  /**
   * Retired ports, by the id of their `port.retired`. Row ids are ULIDs and
   * never reused — a port listed again is a new row with a new slug — so a
   * late event or an older list naming one is ignored rather than reviving it.
   */
  portsRetired: Record<string, number>
  /** Per workspace: the position its port list was last applied at. Absent: not loaded. */
  portsLoaded: Record<string, number>
  /** Whether a preview domain is configured, from the last port list; null until one. */
  previews: boolean | null
  /** Per workspace: its port discovery's state. Absent: nothing said yet, or no scanner. */
  portDiscovery: Record<string, PortDiscovery>
}

export type Action =
  | { type: 'event'; event: StreamEvent }
  | { type: 'resync'; id: number }
  | { type: 'snapshot'; at: number; view: CatalogView }
  | { type: 'workspaces'; at: number; view: WorkspaceList }
  | { type: 'workspace'; at: number; view: WorkspaceDetail }
  | { type: 'secrets'; at: number; view: SecretList }
  | { type: 'identity'; at: number; view: ClaudeIdentityBody }
  | { type: 'resources'; frame: unknown }
  | { type: 'ports'; at: number; workspaceId: string; view: PortList }

export function emptyEntities(): Entities {
  return {
    lastEventId: 0,
    staleSince: null,
    workspaces: {},
    gone: {},
    feeds: {},
    repos: {},
    repoOrder: [],
    installations: {},
    catalogRefreshedAt: null,
    catalogRefreshError: null,
    catalogLoaded: false,
    lastRefresh: null,
    secrets: {},
    secretsDeleted: {},
    secretsLoaded: false,
    secretFault: null,
    secretFaultAt: 0,
    cap: null,
    identity: null,
    identityAt: 0,
    login: null,
    loginAt: 0,
    resources: {},
    hostDisk: null,
    ports: {},
    portsRetired: {},
    portsLoaded: {},
    previews: null,
    portDiscovery: {},
  }
}

export function reduce(prev: Entities, action: Action): Entities {
  switch (action.type) {
    case 'event':
      return applyEvent(prev, action.event)
    case 'resync':
      // The entities cannot be repaired from here — the events that would
      // repair them are gone. Advance to where the server says the stream
      // now is, keep rendering what we have, and say it may be stale until
      // the refetch the store issues lands as a snapshot.
      return {
        ...prev,
        lastEventId: Math.max(prev.lastEventId, action.id),
        staleSince: Math.max(prev.staleSince ?? 0, action.id),
      }
    case 'snapshot':
      return applySnapshot(prev, action.at, action.view)
    case 'workspaces': {
      const next = applyWorkspaceList(prev, action.at, action.view)
      const list = Array.isArray(action.view?.workspaces) ? action.view.workspaces : []
      return withResources(next, Object.fromEntries(list.map((v) => [v.id, v.resources])), action.view?.disk)
    }
    case 'workspace': {
      const next = applyWorkspaceDetail(prev, action.at, action.view)
      return action.view?.id ? withResources(next, { [action.view.id]: action.view.resources }, undefined) : next
    }
    case 'resources': {
      const parts = frameParts(action.frame)
      return parts === null ? prev : withResources(prev, parts.workspaces, parts.host)
    }
    case 'secrets':
      return applySecretList(prev, action.at, action.view)
    case 'ports':
      return applyPortList(prev, action.at, action.workspaceId, action.view)
    case 'identity': {
      // The body stands unless an identity event newer than it was applied.
      const lastEventId = Math.max(prev.lastEventId, action.at)
      let next = prev
      const identity = prev.identityAt <= action.at ? toIdentity(action.view?.identity) : null
      if (identity !== null) next = { ...next, identity, identityAt: action.at }
      // Its login half likewise; a null there is a fact — no login is in
      // progress — and clears an older one.
      if (prev.loginAt <= action.at && action.view !== null && typeof action.view === 'object' && 'login' in action.view) {
        next = { ...next, login: toLogin(action.view.login), loginAt: action.at }
      }
      if (next === prev) return lastEventId === prev.lastEventId ? prev : { ...prev, lastEventId }
      return { ...next, lastEventId }
    }
  }
}

/**
 * The identity, field by named field. A state outside the five is not
 * guessed at: it is "not known" (null), so an unknown verdict can never be
 * rendered as ok. Returns null for a body that is not an identity at all.
 */
function toIdentity(v: unknown): ClaudeIdentity | null {
  if (v === null || typeof v !== 'object') return null
  const x = v as Record<string, unknown>
  const state = (IDENTITY_STATES as readonly unknown[]).includes(x.state) ? x.state as IdentityState : null
  return {
    state,
    accountEmail: str(x.account_email),
    expiresAt: str(x.expires_at),
    loginExpiresAt: str(x.login_expires_at),
    loggedInAt: str(x.logged_in_at),
    lastCheckedAt: str(x.last_checked_at),
    volume: typeof x.volume === 'string' ? x.volume : '',
    checkError: toCheckError(x.check_error),
  }
}

function toCheckError(v: unknown): IdentityCheckError | null {
  if (v === null || typeof v !== 'object') return null
  const x = v as Record<string, unknown>
  const message = str(x.message)
  if (message === null) return null
  return { at: typeof x.at === 'string' ? x.at : '', problem: typeof x.problem === 'string' ? x.problem : '', message }
}

/** The login view, field by named field; null for anything that is not one. */
function toLogin(v: unknown): ClaudeLogin | null {
  if (v === null || typeof v !== 'object') return null
  const x = v as Record<string, unknown>
  const id = str(x.login_id)
  const phase = (LOGIN_PHASES as readonly unknown[]).includes(x.phase) ? x.phase as LoginPhase : null
  if (id === null || phase === null) return null
  return {
    id, phase,
    url: str(x.url),
    deadline: str(x.deadline),
    startedAt: typeof x.started_at === 'string' ? x.started_at : '',
    endedAt: str(x.ended_at),
    attempts: num(x.attempts) ?? 0,
    problem: str(x.problem),
    message: typeof x.message === 'string' ? x.message : '',
  }
}

/** auth.login replaces the login. */
function applyLoginEvent(base: Entities, ev: StreamEvent): Entities {
  if (ev.id <= base.loginAt) return base
  const login = toLogin((ev.data ?? {}).login)
  return login === null ? base : { ...base, login, loginAt: ev.id }
}

/**
 * auth.identity and auth.identity_checked replace the identity — the second
 * is the same view with its last_checked_at moved; auth.identity_check_failed
 * sets only its failure.
 */
function applyIdentityEvent(base: Entities, ev: StreamEvent): Entities {
  if (ev.id <= base.identityAt) return base
  const data = ev.data ?? {}
  if (ev.kind === 'auth.identity' || ev.kind === 'auth.identity_checked') {
    const identity = toIdentity(data.identity)
    return identity === null ? base : { ...base, identity, identityAt: ev.id }
  }
  if (ev.kind === 'auth.identity_check_failed') {
    // A failure the body cannot describe is still a failure: the kind is the
    // fact, and the event's own sentence says it.
    const checkError = toCheckError(data.check_error) ?? { at: ev.at, problem: '', message: ev.message }
    const cur: ClaudeIdentity = base.identity ?? {
      state: null, accountEmail: null, expiresAt: null, loginExpiresAt: null, loggedInAt: null, lastCheckedAt: null, volume: '',
      checkError: null,
    }
    return { ...base, identity: { ...cur, checkError, lastCheckedAt: checkError.at || cur.lastCheckedAt }, identityAt: ev.id }
  }
  return base // an auth.* kind from a later phase
}

/**
 * Applies measurements: a workspace's only while the entities hold it and it
 * is not deleted, and each only if it is from the round held or a later one.
 */
function withResources(
  e: Entities, incoming: Record<string, ResourcesView | null | undefined>, host: HostDiskView | null | undefined,
): Entities {
  const resources = mergeResources(e.resources, incoming, (id) => e.workspaces[id] !== undefined && e.gone[id] === undefined)
  const hostDisk = mergeHostDisk(e.hostDisk, host)
  if (resources === e.resources && hostDisk === e.hostDisk) return e
  return { ...e, resources, hostDisk }
}

/** Folds a recorded sequence; the specs' and the store's shared entry point. */
export function reduceAll(start: Entities, actions: Action[]): Entities {
  return actions.reduce(reduce, start)
}

function isState(v: unknown): v is WorkspaceState {
  return typeof v === 'string' && (WORKSPACE_STATES as readonly string[]).includes(v)
}

function isStepStatus(v: unknown): v is StepStatus {
  return v === 'started' || v === 'done' || v === 'failed' || v === 'needs_approval'
}

/**
 * A request as the server sent it, or null for anything that is not one: a
 * hash and three lists. The settings' values are kept as they came, to be
 * shown as JSON and never interpreted.
 */
function toApproval(v: unknown): ApprovalView | null {
  if (v === null || typeof v !== 'object') return null
  const a = v as Record<string, unknown>
  if (typeof a.hash !== 'string' || a.hash === '') return null
  const list = <T,>(x: unknown): T[] => (Array.isArray(x) ? x.filter((e) => e !== null && typeof e === 'object' && typeof (e as { field?: unknown }).field === 'string') as T[] : [])
  return { hash: a.hash, added: list(a.added), changed: list(a.changed), removed: list(a.removed) }
}

function str(v: unknown): string | null {
  return typeof v === 'string' && v !== '' ? v : null
}

function num(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

function stub(id: string): Workspace {
  return {
    id, repositoryId: null, fullName: null, branch: null, state: null, detail: null, step: null,
    steps: {}, containerId: null, createdAt: null, adopted: false, stateAt: 0, stepAt: 0, containerAt: 0,
    action: null, lastAction: null, stateEventId: 0,
    supervisor: null, supervisorAt: 0, supervisorKnown: false, session: null, sessionAt: 0, approval: null,
  }
}

const ENV_ID = /^env_[A-Za-z0-9]+$/

/** The card's link for an environment id (design §8), or null for anything else. */
export function environmentURL(id: string | null): string | null {
  return id !== null && ENV_ID.test(id) ? `https://claude.ai/code?environment=${id}` : null
}

function toSupervisor(d: Record<string, unknown>, at: string): Supervisor | null {
  const state = d.state
  if (typeof state !== 'string' || !(SUPERVISOR_STATES as readonly string[]).includes(state)) return null
  return {
    state: state as SupervisorState,
    reason: str(d.reason),
    detail: str(d.detail),
    restartCount: num(d.restart_count) ?? 0,
    since: typeof d.at === 'string' ? d.at : at,
  }
}

function toSession(d: Record<string, unknown>, envFallback: string | null): SessionStatus {
  const environmentId = str(d.environment_id) ?? envFallback
  return {
    environmentId,
    url: environmentURL(environmentId),
    capacityUsed: num(d.capacity_used),
    capacityTotal: num(d.capacity_total),
    sessions: num(d.sessions) ?? 0,
  }
}

function applyEvent(prev: Entities, ev: StreamEvent): Entities {
  if (!Number.isInteger(ev.id) || ev.id <= 0) return prev // not an id the server issues
  const lastEventId = Math.max(prev.lastEventId, ev.id)
  const base = lastEventId === prev.lastEventId ? prev : { ...prev, lastEventId }
  const data = ev.data ?? {}

  if (ev.kind === 'repo.refreshed' || ev.kind === 'repo.refresh_failed') {
    if (prev.lastRefresh !== null && prev.lastRefresh.eventId >= ev.id) return base
    const ok = ev.kind === 'repo.refreshed'
    return {
      ...base,
      lastRefresh: {
        ok, at: ev.at, message: ev.message, eventId: ev.id,
        count: ok ? num(data.count) : null,
        added: ok ? num(data.added) : null,
        removed: ok ? num(data.removed) : null,
      },
    }
  }

  if (ev.kind.startsWith('secret.')) return applySecretEvent(base, ev)
  if (ev.kind.startsWith('auth.identity')) return applyIdentityEvent(base, ev)
  if (ev.kind === 'auth.login') return applyLoginEvent(base, ev)

  const wsId = str(ev.workspace_id)
  if (wsId === null) return base
  if (prev.gone[wsId] !== undefined) return base // deleted; nothing brings it back

  if (ev.kind === 'workspace.gone') {
    const workspaces = { ...base.workspaces }
    delete workspaces[wsId]
    const feeds = { ...base.feeds }
    delete feeds[wsId]
    const resources = { ...base.resources }
    delete resources[wsId]
    // Its ports went with it (retired in the same commit, internal/workspace
    // Remove), and the gone tombstone keeps any late port.* event out.
    const ports = Object.fromEntries(Object.entries(base.ports).filter(([, p]) => p.workspaceId !== wsId))
    const portsLoaded = { ...base.portsLoaded }
    delete portsLoaded[wsId]
    const portDiscovery = { ...base.portDiscovery }
    delete portDiscovery[wsId]
    return { ...base, workspaces, feeds, resources, ports, portsLoaded, portDiscovery, gone: { ...base.gone, [wsId]: ev.id } }
  }

  // Every event naming a workspace joins its feed, whatever its kind — but
  // port discovery's (PF §13 step 6): what it finds is the ports panel's, and
  // a test suite opening ports would push the workspace's history out of the
  // feed's 50. GET /api/workspaces/:id leaves them out alike (events.Log.Feed).
  const feed = inFeed(ev) ? mergeFeed(base.feeds[wsId], [ev]) : base.feeds[wsId]
  const fed = feed === base.feeds[wsId] ? base : { ...base, feeds: { ...base.feeds, [wsId]: feed! } }

  if (ev.kind.startsWith('port.')) return applyPortEvent(fed, wsId, ev)

  if (ev.kind === 'supervisor.state' || ev.kind === 'session.status') {
    const known = fed.workspaces[wsId]
    const cur = known ?? stub(wsId)
    let next = cur
    if (ev.kind === 'supervisor.state' && ev.id > cur.supervisorAt) {
      const s = toSupervisor(data, ev.at)
      if (s !== null) next = { ...cur, supervisor: s, supervisorAt: ev.id, supervisorKnown: true }
    }
    if (ev.kind === 'session.status' && ev.id > cur.sessionAt) {
      next = { ...cur, session: toSession(data, cur.session?.environmentId ?? null), sessionAt: ev.id }
    }
    if (next === cur && known !== undefined) return fed
    return { ...fed, workspaces: { ...fed.workspaces, [wsId]: next } }
  }

  if (!ev.kind.startsWith('workspace.')) return fed
  // A job's end (internal/workspace KindJob) says a request is over, which
  // is the in-flight marks' business (stores/workspaces.ts settlesJob), and
  // nothing about the workspace: it joins the feed and changes no entity —
  // not even a stub, since every job's other events name the workspace.
  if (ev.kind === 'workspace.job') return fed

  // Every other workspace.* kind names a workspace that exists. One this
  // client has never heard of becomes a stub, so the event is not lost and
  // the store can see that a refetch is owed (the stub's state is null).
  const known = fed.workspaces[wsId]
  const cur = known ?? stub(wsId)
  let next: Workspace = cur

  switch (ev.kind) {
    case 'workspace.state': {
      if (ev.id > cur.stateEventId) next = { ...next, stateEventId: ev.id }
      // The move to running carries the container's id (design §6), so
      // nothing has to refetch the workspace to learn it. Versioned on its
      // own: a snapshot that already knew the id may have overtaken the state.
      const containerId = str(data.container_id)
      if (containerId !== null && ev.id > cur.containerAt) next = { ...next, containerId, containerAt: ev.id }
      if (!isState(data.state) || ev.id <= cur.stateAt) break
      next = {
        ...next,
        state: data.state,
        detail: str(data.detail),
        stateAt: ev.id,
        repositoryId: num(data.repository_id) ?? cur.repositoryId,
        branch: str(data.branch) ?? cur.branch,
        adopted: data.adopted === true ? true : cur.adopted,
        approval: toApproval(data.approval),
      }
      // A new run through the steps begins at pending (a create); nothing
      // written before it belongs to this workspace's timeline. Start moves
      // stopped or failed to building instead, and each step it reruns
      // overwrites its own row, so the timeline is "latest per step" — the
      // same thing the server's `steps` reports. What a start left behind
      // from an earlier run is trimmed on read, by runSteps.
      if (data.state === 'pending') {
        if (next.stepAt < ev.id) next = { ...next, step: null, stepAt: ev.id }
        const kept = Object.entries(next.steps).filter(([, r]) => r.eventId > ev.id)
        if (kept.length !== Object.keys(next.steps).length) next = { ...next, steps: Object.fromEntries(kept) }
      }
      break
    }
    case 'workspace.step': {
      const name = str(data.step)
      const status = data.status
      if (name === null || !isStepStatus(status)) break
      const detail = str(data.detail)
      // The timeline and the progress line are versioned apart: a late
      // event for one step still lands in its own row after a newer step has
      // moved the progress line on.
      const old = cur.steps[name]
      if (old === undefined || ev.id > old.eventId) {
        next = { ...next, steps: { ...cur.steps, [name]: { status, detail, at: ev.at, eventId: ev.id } } }
      }
      if (ev.id > cur.stepAt) next = { ...next, step: { name, status, detail }, stepAt: ev.id }
      break
    }
    case 'workspace.action':
      next = withAction(next, ev)
      break
    case 'workspace.adopted': {
      // A known row matched to its container at boot; the state is unchanged.
      const containerId = str(data.container_id)
      if (containerId !== null && ev.id > cur.containerAt) next = { ...next, containerId, containerAt: ev.id }
      if (!next.adopted) next = { ...next, adopted: true }
      break
    }
    default:
      // A workspace kind from a later phase: create the stub if it was
      // unknown, change nothing else.
      break
  }

  if (next === cur && known !== undefined) return fed
  return { ...fed, workspaces: { ...fed.workspaces, [wsId]: next } }
}

/**
 * The sub-steps of a stop and a delete, in the order internal/provision runs
 * them. Both begin with the session server, so its `started` is what opens a
 * new run of the same action.
 */
export const ACTION_STEPS: Readonly<Record<string, readonly string[]>> = {
  stop: ['session_server', 'container', 'broker_socket'],
  delete: ['session_server', 'containers', 'broker_socket', 'files'],
}
const FIRST_SUB_STEP = 'session_server'

/**
 * One `workspace.action` event folded into the workspace's action run. A
 * new run begins at a different action, or at the first sub-step's
 * `started`; each sub-step is versioned on its own, as the steps are, so a
 * late event lands in its own row without moving the line back; and an
 * event older than the run it would join belongs to an earlier one and is
 * dropped. Returns `w` itself when nothing changed.
 */
function withAction(prev: Workspace, ev: StreamEvent): Workspace {
  const data = ev.data ?? {}
  const name = str(data.action)
  const step = str(data.step)
  const status = data.status
  if (name === null || step === null || !isStepStatus(status)) return prev
  const detail = str(data.detail)
  const w = prev.lastAction === null || ev.id > prev.lastAction.at
    ? { ...prev, lastAction: { name, step, status, detail, at: ev.id } }
    : prev
  const rec: StepRecord = { status, detail, at: ev.at, eventId: ev.id }
  const run = w.action
  if (run === null || (ev.id > run.eventId && (name !== run.name || (step === FIRST_SUB_STEP && status === 'started')))) {
    return { ...w, action: { name, steps: { [step]: rec }, last: { name: step, status, detail }, startId: ev.id, eventId: ev.id } }
  }
  if (name !== run.name || ev.id < run.startId) return w
  const old = run.steps[step]
  const steps = old === undefined || ev.id > old.eventId ? { ...run.steps, [step]: rec } : run.steps
  const newer = ev.id > run.eventId
  if (steps === run.steps && !newer) return w
  return {
    ...w,
    action: {
      ...run, steps,
      last: newer ? { name: step, status, detail } : run.last,
      eventId: newer ? ev.id : run.eventId,
    },
  }
}

/**
 * The stop or delete still in progress — or one that failed and left the
 * state where it was — or null. A run is current while it is newer than the
 * last state event: a stop ends in the move to stopped, a stuck delete in its
 * annotation, a finished one in `workspace.gone`. And it only ever describes
 * the state it runs in: a stop run on a workspace that is not running is a
 * stale one a snapshot overtook.
 */
export function liveAction(w: Workspace): ActionRun | null {
  const run = w.action
  if (run === null || run.eventId <= w.stateEventId) return null
  // A snapshot newer than the run whose last sub-step failed: a run ends at
  // its first failure, so this one is over, even though the events that
  // ended it — the failure and the annotation after it — fell in a gap.
  const la = w.lastAction
  if (la !== null && la.at > run.eventId && la.status === 'failed') return null
  if (run.name === 'stop' && w.state !== 'running') return null
  if (run.name === 'delete' && w.state !== 'deleting') return null
  return run
}

/**
 * Whether a delete stopped part-way and is waiting to be asked again: the
 * workspace is `deleting`, the server's annotation says why, and no newer
 * sub-step says a resume is already running (design §6: asking again, or the
 * next boot, resumes it).
 */
export function deleteStuck(w: Workspace): boolean {
  return w.state === 'deleting' && w.detail !== null && liveAction(w) === null
}

/**
 * Whether a stop failed and left the workspace running, annotated (design §6,
 * frontend §4.5 #15): `running`, the server's sentence in the detail, no stop
 * in progress, and the latest sub-step a failed stop's. The detail is never
 * parsed — the structure says it was a stop; the sentence is only shown. On a
 * running workspace a detail is written by that annotation or by an orphan's
 * adoption, and any move clears it, so the last sub-step is what tells the
 * two apart. A stop asked for again clears the detail as it starts.
 */
export function stopFailed(w: Workspace): boolean {
  const a = w.lastAction
  return w.state === 'running' && w.detail !== null && liveAction(w) === null &&
    a !== null && a.name === 'stop' && a.status === 'failed'
}

/** Whether an event belongs in a workspace's feed: everything but port discovery's (`data.source: "discovery"`). */
export function inFeed(ev: StreamEvent): boolean {
  return (ev.data as Record<string, unknown> | null | undefined)?.source !== 'discovery'
}

/** Merges events into a feed: by id, newest first, capped. Returns `feed` itself when nothing changed. */
function mergeFeed(feed: StreamEvent[] | undefined, add: StreamEvent[]): StreamEvent[] {
  const cur = feed ?? []
  const have = new Set(cur.map((e) => e.id))
  const fresh = add.filter((e) => Number.isInteger(e.id) && e.id > 0 && !have.has(e.id))
  if (fresh.length === 0) return feed ?? cur
  const out = [...cur, ...fresh].sort((a, b) => b.id - a.id).slice(0, FEED_LIMIT)
  // Everything new fell off the end: nothing visible changed.
  if (feed !== undefined && out.length === cur.length && out.every((e, i) => e === cur[i])) return feed
  return out
}

function toRepo(r: RepoView): Repo {
  return {
    id: r.id,
    installationId: r.installation_id,
    fullName: r.full_name,
    defaultBranch: r.default_branch,
    private: r.private,
    archived: r.archived,
    hasDevcontainer: r.has_devcontainer,
    pushedAt: r.pushed_at,
    removed: r.removed,
  }
}

/**
 * Merges a GET /api/repos body taken when the stream stood at `at`.
 *
 * The body reflects at least every event up to `at` (they were committed
 * before the request was made) and maybe some after. So for each workspace:
 * if an event newer than `at` has already been applied, ours is the fresher
 * fact and is kept; otherwise the snapshot's state wins, at version `at`, so
 * a straggling event from before the snapshot cannot undo it.
 *
 * A workspace the catalog does not mention is kept. The catalog joins only
 * each repository's newest workspace, and never a `deleting` one, so its
 * silence says nothing about whether an older one exists; GET
 * /api/workspaces is what can say that (applyWorkspaceList).
 *
 * Repositories carry no event ids; the snapshot is their only source, so it
 * replaces them outright.
 */
function applySnapshot(prev: Entities, at: number, view: CatalogView): Entities {
  const repos: Record<number, Repo> = {}
  const repoOrder: number[] = []
  const installations: Record<number, InstallationView> = {}
  for (const i of view.installations) installations[i.id] = i

  const workspaces: Record<string, Workspace> = { ...prev.workspaces }
  for (const r of view.repos) {
    repos[r.id] = toRepo(r)
    repoOrder.push(r.id)
    const w = r.workspace
    if (w === null || !isState(w.state)) continue
    const gone = prev.gone[w.id]
    if (gone !== undefined) continue // the snapshot predates the delete, or races it
    const cur = prev.workspaces[w.id]
    if (cur !== undefined && cur.stateAt > at) {
      workspaces[w.id] = cur.repositoryId === null ? { ...cur, repositoryId: r.id, fullName: r.full_name } : cur
    } else if (cur !== undefined && cur.state === w.state) {
      workspaces[w.id] = { ...cur, repositoryId: r.id, fullName: cur.fullName ?? r.full_name, stateAt: Math.max(cur.stateAt, at) }
    } else {
      workspaces[w.id] = {
        ...(cur ?? stub(w.id)),
        repositoryId: r.id, fullName: r.full_name, state: w.state, detail: null, stateAt: at,
      }
    }
  }

  return {
    ...prev,
    lastEventId: Math.max(prev.lastEventId, at),
    staleSince: prev.staleSince !== null && at < prev.staleSince ? prev.staleSince : null,
    workspaces,
    repos,
    repoOrder,
    installations,
    catalogRefreshedAt: view.refreshed_at,
    catalogRefreshError: view.last_refresh_error,
    catalogLoaded: true,
  }
}

/**
 * One workspace from a GET /api/workspaces(/:id) body taken at `at`, merged
 * over what the entities already hold. The rule is applySnapshot's: a fact an
 * event newer than `at` wrote is kept, anything else takes the body's,
 * versioned at `at`. State and its detail move together; each step is
 * versioned on its own, so a step event that beat the body keeps its row.
 */
function mergeView(cur: Workspace | undefined, at: number, v: WorkspaceView): Workspace {
  const base = cur ?? stub(v.id)
  const steps: Record<string, StepRecord> = { ...base.steps }
  for (const [name, sv] of Object.entries(v.steps ?? {})) {
    if (!isStepStatus(sv.status)) continue
    const old = steps[name]
    if (old !== undefined && old.eventId > at) continue
    steps[name] = { status: sv.status, detail: str(sv.detail), at: sv.at, eventId: at }
  }
  const eventsWin = base.stateAt > at
  const containerWins = base.containerAt > at
  // The snapshot moved the state past a run we saw part of: that run is over,
  // and whatever ended it fell in a gap. A detail body puts back what it can.
  const staleRun = !eventsWin && base.state !== v.state && base.action !== null && base.action.eventId <= at
  // The latest sub-step, by the same rule as every field: an event newer than
  // the body keeps its own. A body without the field (an older server) says
  // nothing either way.
  const la = v.last_action
  const lastAction = la === undefined || (base.lastAction !== null && base.lastAction.at > at)
    ? base.lastAction
    : la === null || !isStepStatus(la.status) || str(la.action) === null || str(la.step) === null
      ? null
      : { name: la.action, step: la.step, status: la.status, detail: str(la.detail), at }
  // The supervisor and session halves, by the same rule. A body without the
  // fields (an older server) says nothing either way.
  let supervisor = base.supervisor
  let supervisorAt = base.supervisorAt
  const supervisorKnown = base.supervisorKnown || v.supervisor !== undefined
  if (v.supervisor !== undefined && base.supervisorAt <= at) {
    supervisor = v.supervisor === null ? null : toSupervisor(v.supervisor as unknown as Record<string, unknown>, v.supervisor.at)
    supervisorAt = at
  }
  let session = base.session
  let sessionAt = base.sessionAt
  if ((v.session !== undefined || v.environment_id !== undefined) && base.sessionAt <= at) {
    const env = str(v.environment_id)
    session = v.session == null
      ? (env === null ? null : toSession({}, env))
      : toSession(v.session as unknown as Record<string, unknown>, env)
    sessionAt = at
  }
  return {
    ...base,
    supervisor, supervisorAt, supervisorKnown, session, sessionAt,
    action: staleRun ? null : base.action,
    lastAction,
    repositoryId: v.repository_id,
    fullName: v.full_name,
    branch: v.branch,
    state: eventsWin ? base.state : v.state,
    detail: eventsWin ? base.detail : v.state_detail,
    // A body without the field (an older server) has no requests to show.
    approval: eventsWin ? base.approval : toApproval(v.approval ?? null),
    stateAt: eventsWin ? base.stateAt : at,
    containerId: containerWins ? base.containerId : v.container_id,
    containerAt: containerWins ? base.containerAt : at,
    createdAt: v.created_at,
    steps,
  }
}

function isView(v: WorkspaceView): boolean {
  return typeof v.id === 'string' && v.id !== '' && isState(v.state)
}

/**
 * Merges a GET /api/workspaces body: every workspace with a row. Unlike the
 * catalog it is the authority on existence — a workspace it does not list,
 * and that no event after `at` has touched, is gone from the server and is
 * dropped. That is also how a stub the refetch could not explain stops
 * asking for refetches.
 */
function applyWorkspaceList(prev: Entities, at: number, view: WorkspaceList): Entities {
  const workspaces: Record<string, Workspace> = {}
  for (const v of view.workspaces) {
    if (!isView(v) || prev.gone[v.id] !== undefined) continue
    workspaces[v.id] = mergeView(prev.workspaces[v.id], at, v)
  }
  for (const [id, w] of Object.entries(prev.workspaces)) {
    if (workspaces[id] !== undefined) continue
    if (w.stateAt > at || w.stepAt > at) workspaces[id] = w
  }
  const feeds: Record<string, StreamEvent[]> = {}
  for (const [id, f] of Object.entries(prev.feeds)) {
    if (workspaces[id] !== undefined || f.some((e) => e.id > at)) feeds[id] = f
  }
  return {
    ...prev,
    lastEventId: Math.max(prev.lastEventId, at),
    staleSince: prev.staleSince !== null && at < prev.staleSince ? prev.staleSince : null,
    workspaces,
    feeds,
    // A body without the field (an older server) leaves what was known.
    ...(view.capacity !== undefined ? { cap: num(view.capacity.cap) } : {}),
  }
}

/**
 * Merges a GET /api/workspaces/:id body: the one workspace, and its recent
 * events into its feed. It names one workspace, so it drops none — and it
 * does not clear a resync, because the rest of the entities are no fresher
 * for it.
 */
function applyWorkspaceDetail(prev: Entities, at: number, view: WorkspaceDetail): Entities {
  if (!isView(view) || prev.gone[view.id] !== undefined) {
    return at > prev.lastEventId ? { ...prev, lastEventId: at } : prev
  }
  const events = (view.events ?? []).filter((e) => str(e.workspace_id) === view.id && inFeed(e))
  // The body's events are the stream's own, ids and all, so the stop or
  // delete they tell — and the state event that ended it, if one did — fold
  // in exactly as they would have live. That is what lets a reload show a
  // stuck delete's sub-steps, or a stop that failed.
  let w = mergeView(prev.workspaces[view.id], at, view)
  for (const e of [...events].sort((a, b) => a.id - b.id)) {
    if (!Number.isInteger(e.id) || e.id <= 0) continue
    if (e.kind === 'workspace.state' && e.id > w.stateEventId) w = { ...w, stateEventId: e.id }
    else if (e.kind === 'workspace.action') w = withAction(w, e)
  }
  return {
    ...prev,
    lastEventId: Math.max(prev.lastEventId, at),
    workspaces: { ...prev.workspaces, [view.id]: w },
    feeds: { ...prev.feeds, [view.id]: mergeFeed(prev.feeds[view.id], events) },
  }
}

const SECRET_WRITES = new Set(['secret.created', 'secret.updated', 'secret.rotated', 'secret.grants'])

function strOrNull(v: unknown): string | null {
  return typeof v === 'string' ? v : null
}

/**
 * A secret's metadata, field by named field. Nothing is spread from the
 * input, so a field this type does not name — a `value`, were a server bug
 * ever to send one — cannot reach the store, and so cannot reach a template.
 */
function toSecret(m: unknown, at: number): Secret | null {
  if (m === null || typeof m !== 'object') return null
  const v = m as Partial<Record<keyof SecretMeta, unknown>>
  const name = str(v.name)
  if (name === null || typeof v.reach !== 'string') return null
  const grants = Array.isArray(v.grants) ? v.grants.flatMap((g: unknown) => {
    const x = g as { repository_id?: unknown; full_name?: unknown } | null
    const id = num(x?.repository_id)
    return id === null ? [] : [{ repositoryId: id, fullName: typeof x?.full_name === 'string' ? x.full_name : '' }]
  }) : []
  return {
    name,
    reach: v.reach,
    description: typeof v.description === 'string' ? v.description : '',
    allRepos: v.all_repos === true,
    grants,
    createdAt: strOrNull(v.created_at),
    rotatedAt: strOrNull(v.rotated_at),
    lastAccessAt: strOrNull(v.last_access_at),
    accessedBy: Array.isArray(v.accessed_by) ? v.accessed_by.filter((x): x is string => typeof x === 'string') : [],
    at,
  }
}

/**
 * The delivery fault, named fields only. An undeliverable report whose body
 * cannot be read is still a fault — the event's kind is the fact, and
 * dropping it would hide the one fleet-wide failure Phase 4 has — so it
 * becomes one with no names, dated by the event.
 */
function toFault(u: unknown, fallbackSince: string | null): SecretFault {
  const v = (u !== null && typeof u === 'object' ? u : {}) as { since?: unknown; secrets?: unknown }
  const secrets = Array.isArray(v.secrets) ? v.secrets.flatMap((s: unknown) => {
    const x = s as { name?: unknown; reason?: unknown } | null
    const name = str(x?.name)
    return name === null ? [] : [{ name, reason: typeof x?.reason === 'string' ? x.reason : '' }]
  }) : []
  return { since: str(v.since) ?? fallbackSince, secrets }
}

/** The secret.* events. Versioned per name, and against the name's last delete. */
function applySecretEvent(base: Entities, ev: StreamEvent): Entities {
  const data = ev.data ?? {}
  if (ev.kind === 'secret.undeliverable' || ev.kind === 'secret.deliverable') {
    if (ev.id <= base.secretFaultAt) return base
    const fault = ev.kind === 'secret.deliverable' ? null : toFault(data.undeliverable, ev.at)
    return { ...base, secretFault: fault, secretFaultAt: ev.id }
  }
  if (ev.kind === 'secret.deleted') {
    const name = str(data.name)
    if (name === null) return base
    const cur = base.secrets[name]
    if (cur !== undefined && cur.at >= ev.id) return base // recreated after this delete
    const secrets = { ...base.secrets }
    delete secrets[name]
    return { ...base, secrets, secretsDeleted: { ...base.secretsDeleted, [name]: Math.max(ev.id, base.secretsDeleted[name] ?? 0) } }
  }
  if (!SECRET_WRITES.has(ev.kind)) return base // a secret.* kind from a later phase
  const s = toSecret(data.secret, ev.id)
  if (s === null) return base
  const cur = base.secrets[s.name]
  if (cur !== undefined && cur.at >= ev.id) return base
  if ((base.secretsDeleted[s.name] ?? 0) >= ev.id) return base
  return { ...base, secrets: { ...base.secrets, [s.name]: s } }
}

/**
 * Merges a GET /api/secrets body taken when the stream stood at `at`. The
 * list names every secret, so it is the authority on which exist — a secret
 * it omits is dropped, unless an event newer than `at` wrote it. A secret an
 * event newer than `at` wrote keeps the event's version; one deleted after
 * `at` stays deleted. Last access is the one thing only the list carries
 * fresh (a fetch writes `secret_access`, not an event), so it is why the list
 * is refetched on entry.
 */
function applySecretList(prev: Entities, at: number, view: SecretList): Entities {
  const secrets: Record<string, Secret> = {}
  for (const m of view.secrets ?? []) {
    const s = toSecret(m, at)
    if (s === null) continue
    if ((prev.secretsDeleted[s.name] ?? 0) > at) continue
    const cur = prev.secrets[s.name]
    secrets[s.name] = cur !== undefined && cur.at > at ? cur : s
  }
  for (const [name, s] of Object.entries(prev.secrets)) {
    if (secrets[name] === undefined && s.at > at) secrets[name] = s
  }
  // The delivery fault, by the same rule: the snapshot's answer stands unless
  // a delivery event newer than it was applied. A body without the field (a
  // server older than §4.5 #12) says nothing about it either way.
  const fault = 'undeliverable' in view && prev.secretFaultAt <= at
    ? { secretFault: view.undeliverable == null ? null : toFault(view.undeliverable, null), secretFaultAt: at }
    : {}
  return {
    ...prev,
    lastEventId: Math.max(prev.lastEventId, at),
    secrets,
    secretsLoaded: true,
    ...fault,
  }
}

const PORT_WRITES = new Set(['port.added', 'port.enabled', 'port.disabled', 'port.updated'])

/**
 * A port row, field by named field, or null for one that is not a port. A
 * `host_header` outside the two is read as the default, `localhost`; the URL
 * is taken only from an https URL, so nothing on the wire can make the link
 * the panel renders point at another scheme.
 */
function toPort(m: unknown, at: number): Port | null {
  if (m === null || typeof m !== 'object') return null
  const v = m as Partial<Record<keyof PortView, unknown>>
  const id = str(v.id)
  const workspaceId = str(v.workspace_id)
  const containerPort = num(v.container_port)
  const slug = str(v.slug)
  if (id === null || workspaceId === null || containerPort === null || slug === null) return null
  const url = str(v.url)
  return {
    id, workspaceId, containerPort, slug,
    host: str(v.host),
    url: url !== null && url.startsWith('https://') ? url : null,
    label: str(v.label),
    hostHeader: v.host_header === 'passthrough' ? 'passthrough' : 'localhost',
    enabled: v.enabled === true,
    hidden: v.hidden === true,
    declared: v.declared === true,
    observed: v.observed === true,
    manual: v.manual === true,
    bindAddr: str(v.bind_addr),
    loopback: v.loopback === true,
    observedState: v.observed_state === 'listening' || v.observed_state === 'gone' ? v.observed_state : null,
    lastSeenAt: str(v.last_seen_at),
    at,
  }
}

const DISCOVERY_STATES: readonly string[] = ['ok', 'unavailable', 'limited']

function toDiscovery(v: unknown): DiscoveryState | null {
  return typeof v === 'string' && DISCOVERY_STATES.includes(v) ? (v as DiscoveryState) : null
}

/**
 * The port.* events (internal/preview): every kind but port.retired carries
 * the whole row as `data.port`, so it is an upsert versioned by event id;
 * port.retired drops the row for good. Discovery's (`data.source:
 * "discovery"`) are the same kinds and are applied alike: a row the scan
 * found is a row, never a notice. port.scanned (which answers a rescan, and
 * stores/ports.ts settles on) and port.discovery change no row: each reports
 * the workspace's discovery state, which the panel says (PF §11); a kind from
 * a later phase changes nothing.
 */
function applyPortEvent(base: Entities, wsId: string, ev: StreamEvent): Entities {
  const data = ev.data ?? {}
  if (ev.kind === 'port.scanned' || ev.kind === 'port.discovery') {
    const state = toDiscovery(data.discovery)
    const cur = base.portDiscovery[wsId]
    if (state === null || (cur !== undefined && cur.at >= ev.id)) return base
    return { ...base, portDiscovery: { ...base.portDiscovery, [wsId]: { state, at: ev.id } } }
  }
  if (ev.kind === 'port.retired') {
    const id = str(data.port_id)
    if (id === null) return base
    const ports = { ...base.ports }
    delete ports[id]
    return { ...base, ports, portsRetired: { ...base.portsRetired, [id]: Math.max(ev.id, base.portsRetired[id] ?? 0) } }
  }
  if (!PORT_WRITES.has(ev.kind)) return base
  const p = toPort(data.port, ev.id)
  if (p === null || p.workspaceId !== wsId) return base
  if (base.portsRetired[p.id] !== undefined) return base
  const cur = base.ports[p.id]
  if (cur !== undefined && cur.at >= ev.id) return base
  return { ...base, ports: { ...base.ports, [p.id]: p } }
}

/**
 * Merges a GET /api/workspaces/:id/ports?hidden=true body taken when the
 * stream stood at `at`. It names every live port of that workspace, so it is
 * the authority on which exist there — a port it omits is dropped unless an
 * event newer than `at` wrote it — and it never revives a retired one.
 */
function applyPortList(prev: Entities, at: number, wsId: string, view: PortList): Entities {
  if (prev.gone[wsId] !== undefined || view === null || typeof view !== 'object') {
    return at > prev.lastEventId ? { ...prev, lastEventId: at } : prev
  }
  const ports: Record<string, Port> = {}
  for (const [id, p] of Object.entries(prev.ports)) {
    if (p.workspaceId !== wsId || p.at > at) ports[id] = p
  }
  for (const m of Array.isArray(view.ports) ? view.ports : []) {
    const p = toPort(m, at)
    if (p === null || p.workspaceId !== wsId || prev.portsRetired[p.id] !== undefined) continue
    const cur = prev.ports[p.id]
    ports[p.id] = cur !== undefined && cur.at > at ? cur : p
  }
  // The list's discovery is as fresh as the request, so it stands over any
  // report the stream had delivered by `at`; a newer event stands over it.
  const state = toDiscovery(view.discovery)
  const cur = prev.portDiscovery[wsId]
  const portDiscovery = state !== null && (cur === undefined || cur.at <= at)
    ? { ...prev.portDiscovery, [wsId]: { state, at } }
    : prev.portDiscovery
  return {
    ...prev,
    lastEventId: Math.max(prev.lastEventId, at),
    ports,
    portsLoaded: { ...prev.portsLoaded, [wsId]: at },
    previews: typeof view.previews === 'boolean' ? view.previews : prev.previews,
    portDiscovery,
  }
}

/** A workspace's live ports, by port number. */
export function portsOf(e: Entities, wsId: string): Port[] {
  return Object.values(e.ports).filter((p) => p.workspaceId === wsId).sort((a, b) => a.containerPort - b.containerPort)
}

/** Secrets by name, for the list. */
export function secretList(e: Entities): Secret[] {
  return Object.values(e.secrets).sort((a, b) => a.name.localeCompare(b.name))
}

const stepOrder = (n: string) => (WORKSPACE_STEPS as readonly string[]).indexOf(n)
const time = (iso: string) => {
  const t = Date.parse(iso)
  return Number.isNaN(t) ? -Infinity : t
}

/** Whether step record `a` happened after `b`: by the server's time, then event id, then run order. */
function later(a: [string, StepRecord], b: [string, StepRecord]): boolean {
  const ta = time(a[1].at)
  const tb = time(b[1].at)
  if (ta !== tb) return ta > tb
  if (a[1].eventId !== b[1].eventId) return a[1].eventId > b[1].eventId
  return stepOrder(a[0]) > stepOrder(b[0])
}

/** The latest-written step, or null for an empty timeline. */
function latestStep(steps: Record<string, StepRecord>): [string, StepRecord] | null {
  let best: [string, StepRecord] | null = null
  for (const entry of Object.entries(steps)) if (best === null || later(entry, best)) best = entry
  return best
}

/**
 * The step timeline of the current run (frontend §6.1, *Steps after a
 * start*). `steps` holds the latest event per step — the server's `steps`
 * says the same — and a start reruns only from step 3 (or 2), so a step the
 * earlier run reached and this one has not yet keeps the earlier run's
 * status: a `failed` from the last attempt, or a `done` from before a stop.
 *
 * The rule: find the latest-written step, L. Every run walks the steps in
 * order, so a step *after* L in run order that was written *before* L
 * belongs to an earlier run, and is left out — shown as not run. Steps
 * before L in run order are kept, whether this run wrote them or they are
 * the prefix it resumed after (allocate, clone): those happened, and their
 * result is what the run is standing on. A create clears the timeline
 * outright (the `pending` case in applyEvent), so this only ever trims what
 * a start left behind.
 *
 * Time is the server's `at`, not the event id: a snapshot versions every row
 * at its own position, so ids cannot order rows that came from one.
 */
export function runSteps(w: Workspace): Record<string, StepRecord> {
  const last = latestStep(w.steps)
  if (last === null) return w.steps
  const lastOrder = stepOrder(last[0])
  const lastAt = time(last[1].at)
  const kept = Object.entries(w.steps).filter(([name, r]) => !(stepOrder(name) > lastOrder && time(r.at) < lastAt))
  return kept.length === Object.keys(w.steps).length ? w.steps : Object.fromEntries(kept)
}

/**
 * The step the card names: the most recent step event, or — when only a
 * snapshot has been seen — the latest-written row of the current run.
 */
export function currentStep(w: Workspace): StepInfo | null {
  if (w.step !== null) return w.step
  const best = latestStep(runSteps(w))
  return best === null ? null : { name: best[0], status: best[1].status, detail: best[1].detail }
}

/** The step a failed workspace failed at in its current run, or null when nothing says. */
export function failedStep(w: Workspace): string | null {
  if (w.step?.status === 'failed') return w.step.name
  const failed = Object.fromEntries(Object.entries(runSteps(w)).filter(([, r]) => r.status === 'failed'))
  return latestStep(failed)?.[0] ?? null
}

/** The newest workspace holding a repository: the join the home list shows. */
export function workspaceForRepo(e: Entities, repoId: number): Workspace | null {
  let best: Workspace | null = null
  for (const w of Object.values(e.workspaces)) {
    if (w.repositoryId !== repoId || w.state === null) continue
    // ULIDs sort by creation time, which is the server's own "newest" (view.go).
    if (best === null || w.id > best.id) best = w
  }
  return best
}

/**
 * Whether any workspace at all holds a repository — in any state, a stub
 * whose state is not yet known included. The server refuses a second create
 * for a repository with a workspace row in any state (design §5: one
 * repository, one workspace, until a delete finishes), so this, not
 * `workspaceForRepo`, is what decides whether Clone is offered.
 */
export function repoHeld(e: Entities, repoId: number): boolean {
  return Object.values(e.workspaces).some((w) => w.repositoryId === repoId)
}

/** Whether the store owes a refetch: something named a workspace it cannot place. */
export function hasStubs(e: Entities): boolean {
  return Object.values(e.workspaces).some((w) => w.state === null || w.repositoryId === null)
}
