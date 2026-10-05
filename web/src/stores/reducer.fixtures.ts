// Recorded event sequences for the reducer's specs (frontend §10): what the
// server writes, frame for frame, for the flows Phase 2 has. Each frame's
// `data` is exactly what the Go Emit call marshals — internal/workspace
// (Create, Move, stepEvent, Remove, Adopt), internal/reconcile,
// internal/catalog and internal/broker, and Phase 6's internal/provision
// lifecycle (subSteps, Annotate) — so a reducer test over these is a
// test against the server's shapes, not against our memory of them.

import type { CatalogView, StepView, StreamEvent, WorkspaceDetail, WorkspaceList, WorkspaceView } from '../api/types'

export const WS = '01JA0000000000000000000010'
export const WS2 = '01JA0000000000000000000011'
export const ORPHAN = '01JA0000000000000000000012'

const at = (n: number) => new Date(Date.UTC(2026, 9, 4, 12, 0, n)).toISOString()

function ev(id: number, kind: string, workspace_id: string | undefined, data: Record<string, unknown> | undefined, message = kind, level: StreamEvent['level'] = 'info'): StreamEvent {
  return {
    id, kind, message, level, at: at(id),
    ...(workspace_id !== undefined ? { workspace_id } : {}),
    ...(data !== undefined ? { data } : {}),
  }
}

/** A successful clone of repo 1: Create, the eight steps, and the moves between them. */
export const CLONE_OK: StreamEvent[] = [
  ev(1, 'workspace.state', WS, { state: 'pending', repository_id: 1, branch: 'main' }, 'Workspace created.'),
  ev(2, 'workspace.step', WS, { step: 'allocate', status: 'started' }),
  ev(3, 'workspace.step', WS, { step: 'allocate', status: 'done' }),
  ev(4, 'workspace.state', WS, { state: 'cloning', from: 'pending' }, 'Cloning.'),
  ev(5, 'workspace.step', WS, { step: 'clone', status: 'started' }),
  ev(6, 'workspace.step', WS, { step: 'clone', status: 'done' }),
  ev(7, 'workspace.step', WS, { step: 'resolve_config', status: 'started' }),
  ev(8, 'workspace.step', WS, { step: 'resolve_config', status: 'done' }),
  ev(9, 'workspace.state', WS, { state: 'building', from: 'cloning' }, 'Building the container.'),
  ev(10, 'workspace.step', WS, { step: 'up', status: 'started' }),
  ev(11, 'token.issued', WS, { scope: 'git', expires_at: at(3600) }, 'Issued a git token.'),
  ev(12, 'workspace.step', WS, { step: 'up', status: 'done' }),
  ev(13, 'workspace.state', WS, { state: 'running', from: 'building', container_id: 'c0ffee0123456789' }, 'Running.'),
]

/** A clone of repo 2 that fails at `up`: the step names itself, then the move to failed. */
export const CLONE_FAILS_AT_UP: StreamEvent[] = [
  ev(20, 'workspace.state', WS2, { state: 'pending', repository_id: 2, branch: 'main' }, 'Workspace created.'),
  ev(21, 'workspace.state', WS2, { state: 'cloning', from: 'pending' }),
  ev(22, 'workspace.state', WS2, { state: 'building', from: 'cloning' }),
  ev(23, 'workspace.step', WS2, { step: 'up', status: 'started' }),
  ev(24, 'workspace.step', WS2, { step: 'up', status: 'failed', detail: 'The container start step failed.' },
    'The container start step failed.', 'error'),
  ev(25, 'workspace.state', WS2, { state: 'failed', from: 'building', detail: 'The container start step failed.' },
    'Failed. The container start step failed.', 'error'),
]

/** Deleting WS after CLONE_OK. */
export const DELETE: StreamEvent[] = [
  ev(30, 'workspace.state', WS, { state: 'deleting', from: 'running' }, 'Deleting.'),
  ev(31, 'workspace.gone', WS, {}, 'Workspace deleted.'),
]

/** Boot reconciliation: an orphan adopted from its labels, a known row matched, a stranger left alone. */
export const RECONCILE: StreamEvent[] = [
  ev(40, 'workspace.state', ORPHAN, {
    state: 'running', adopted: true, repository_id: 3, branch: 'main',
    detail: 'Found with no record; adopted from its labels.',
  }, 'Running. Found with no record; adopted from its labels.', 'warn'),
  ev(41, 'workspace.adopted', WS, { container_id: 'c0ffee' }, 'Found running after a restart.'),
  ev(42, 'container.unclaimed', undefined, { container_id: 'beef', reason: 'leave_orphan' },
    'A container carries this instance\'s label but could not be matched to a workspace; it was left alone.', 'warn'),
  ev(43, 'system.reconcile', undefined, undefined,
    'Could not reconcile workspaces with Docker at startup; nothing was changed. See the service log.', 'warn'),
]

export const REFRESHED = (id: number, count = 4): StreamEvent =>
  ev(id, 'repo.refreshed', undefined, { count, added: 1, removed: 0 }, `Repository list refreshed: ${count} repositories.`)

export const REFRESH_FAILED = (id: number): StreamEvent =>
  ev(id, 'repo.refresh_failed', undefined, {}, 'Could not refresh the repository list from GitHub: Bad credentials', 'warn')

export const stateEvent = (id: number, ws: string, state: string, extra: Record<string, unknown> = {}): StreamEvent =>
  ev(id, 'workspace.state', ws, { state, ...extra })

/** A `token.issued` naming a workspace: changes no entity, joins the feed. */
export const tokenIssued = (id: number, ws: string): StreamEvent =>
  ev(id, 'token.issued', ws, { scope: 'gh', expires_at: at(id + 3600) }, 'Issued a gh token.')

export const stepEvent = (id: number, ws: string, step: string, status: string, detail?: string): StreamEvent =>
  ev(id, 'workspace.step', ws, { step, status, ...(detail !== undefined ? { detail } : {}) },
    status === 'failed' ? detail ?? step : `${step} ${status}`, status === 'failed' ? 'error' : 'info')

/** A start of WS2 after CLONE_FAILS_AT_UP: failed → building, `up` rerun, running. */
export const START_AFTER_FAIL: StreamEvent[] = [
  ev(50, 'workspace.state', WS2, { state: 'building', from: 'failed' }, 'Building the container.'),
  ev(51, 'workspace.step', WS2, { step: 'up', status: 'started' }),
  ev(52, 'workspace.step', WS2, { step: 'up', status: 'done' }),
  ev(53, 'workspace.state', WS2, { state: 'running', from: 'building', container_id: 'feed0123456789ab' }, 'Running.'),
]

/**
 * WS after CLONE_OK: stopped, then a start that resumes at step 3 and fails
 * there. `up` keeps its `done` from the first run (event 12) — the server's
 * `steps` is the latest per step — and is not part of this run.
 */
export const RESTART_FAILS_EARLY: StreamEvent[] = [
  ev(60, 'workspace.state', WS, { state: 'stopped', from: 'running' }, 'Stopped.'),
  ev(61, 'workspace.state', WS, { state: 'building', from: 'stopped' }, 'Building the container.'),
  ev(62, 'workspace.step', WS, { step: 'resolve_config', status: 'started' }),
  ev(63, 'workspace.step', WS, { step: 'resolve_config', status: 'failed', detail: 'Could not read the dev container configuration.' },
    'Could not read the dev container configuration.', 'error'),
  ev(64, 'workspace.state', WS, { state: 'failed', from: 'building', detail: 'Could not read the dev container configuration.' },
    'Failed. Could not read the dev container configuration.', 'error'),
]

/**
 * One `workspace.action` frame, as internal/provision subSteps writes it:
 * `{action, step, status, detail?}`, the message "<Action>: <step> <status>."
 * with the detail after it, and level error for a failure.
 */
export const actionEvent = (id: number, ws: string, action: string, sub: string, status: string, detail?: string): StreamEvent => {
  const word = action.charAt(0).toUpperCase() + action.slice(1)
  return ev(id, 'workspace.action', ws, { action, step: sub, status, ...(detail !== undefined ? { detail } : {}) },
    detail !== undefined ? `${word}: ${sub} ${status}. ${detail}` : `${word}: ${sub} ${status}.`,
    status === 'failed' ? 'error' : 'info')
}

const NO_SUPERVISOR = 'Nothing to do yet: the Claude Code session server arrives with Claude support.'

/** WS after CLONE_OK, stopped: the three sub-steps, then running → stopped. */
export const STOP_OK: StreamEvent[] = [
  actionEvent(70, WS, 'stop', 'session_server', 'started'),
  actionEvent(71, WS, 'stop', 'session_server', 'done', NO_SUPERVISOR),
  actionEvent(72, WS, 'stop', 'container', 'started'),
  actionEvent(73, WS, 'stop', 'container', 'done'),
  actionEvent(74, WS, 'stop', 'broker_socket', 'started'),
  actionEvent(75, WS, 'stop', 'broker_socket', 'done'),
  ev(76, 'workspace.state', WS, { state: 'stopped', from: 'running' }, 'Stopped.'),
]

/** internal/provision StopFailedDetail for `docker stop` failing. */
export const STOP_FAILED_DETAIL = "The stop did not finish: docker could not stop the workspace's container. Stop again to retry."

/**
 * WS after CLONE_OK: a stop whose `docker stop` fails. It stays running, and
 * Annotate writes a running → running state event whose detail names the
 * sub-step (frontend §4.5 #15).
 */
export const STOP_FAILS: StreamEvent[] = [
  actionEvent(80, WS, 'stop', 'session_server', 'started'),
  actionEvent(81, WS, 'stop', 'session_server', 'done', NO_SUPERVISOR),
  actionEvent(82, WS, 'stop', 'container', 'started'),
  actionEvent(83, WS, 'stop', 'container', 'failed', "docker could not stop the workspace's container."),
  ev(84, 'workspace.state', WS, { state: 'running', from: 'running', detail: STOP_FAILED_DETAIL },
    `Running. ${STOP_FAILED_DETAIL}`, 'warn'),
]

/** STOP_FAILS asked again: ClearDetail as it starts, then the sub-steps, then stopped. */
export const STOP_RETRIED: StreamEvent[] = [
  ev(85, 'workspace.state', WS, { state: 'running', from: 'running' }, 'Running.'),
  actionEvent(86, WS, 'stop', 'session_server', 'started'),
  actionEvent(87, WS, 'stop', 'session_server', 'done', NO_SUPERVISOR),
  actionEvent(88, WS, 'stop', 'container', 'started'),
  actionEvent(89, WS, 'stop', 'container', 'done'),
]

export const STUCK_DETAIL = "The delete stopped part-way: Drydock could not remove the workspace's directory; files inside may belong to another user. Delete again to retry."

/**
 * WS after CLONE_OK: a delete that sticks at `files`. The move to deleting,
 * three sub-steps done, `files` failed, and then Annotate's deleting →
 * deleting state event, whose detail names the sub-step.
 */
export const DELETE_STUCK: StreamEvent[] = [
  ev(90, 'workspace.state', WS, { state: 'deleting', from: 'running' }, 'Deleting.'),
  actionEvent(91, WS, 'delete', 'session_server', 'started'),
  actionEvent(92, WS, 'delete', 'session_server', 'done', NO_SUPERVISOR),
  actionEvent(93, WS, 'delete', 'containers', 'started'),
  actionEvent(94, WS, 'delete', 'containers', 'done', 'Removed its container.'),
  actionEvent(95, WS, 'delete', 'broker_socket', 'started'),
  actionEvent(96, WS, 'delete', 'broker_socket', 'done'),
  actionEvent(97, WS, 'delete', 'files', 'started'),
  actionEvent(98, WS, 'delete', 'files', 'failed',
    "Drydock could not remove the workspace's directory; files inside may belong to another user."),
  ev(99, 'workspace.state', WS, { state: 'deleting', from: 'deleting', detail: STUCK_DETAIL }, `Deleting. ${STUCK_DETAIL}`, 'warn'),
]

/**
 * DELETE_STUCK asked again: no move (it is deleting already) but ClearDetail's
 * deleting → deleting state event with no detail as it starts (frontend §4.5
 * #16), every sub-step rerun — each is idempotent — and then the row goes.
 */
export const DELETE_RESUMED: StreamEvent[] = [
  ev(100, 'workspace.state', WS, { state: 'deleting', from: 'deleting' }, 'Deleting.'),
  actionEvent(101, WS, 'delete', 'session_server', 'started'),
  actionEvent(102, WS, 'delete', 'session_server', 'done', NO_SUPERVISOR),
  actionEvent(103, WS, 'delete', 'containers', 'started'),
  actionEvent(104, WS, 'delete', 'containers', 'done', 'No container to remove.'),
  actionEvent(105, WS, 'delete', 'broker_socket', 'started'),
  actionEvent(106, WS, 'delete', 'broker_socket', 'done'),
  actionEvent(107, WS, 'delete', 'files', 'started'),
  actionEvent(108, WS, 'delete', 'files', 'done'),
  ev(109, 'workspace.gone', WS, {}, 'Workspace deleted.'),
]

/** WS2 after CLONE_FAILS_AT_UP, deleted in one go: the move, four sub-steps, the gone. */
export const DELETE_OK: StreamEvent[] = [
  ev(110, 'workspace.state', WS2, { state: 'deleting', from: 'failed' }, 'Deleting.'),
  actionEvent(111, WS2, 'delete', 'session_server', 'started'),
  actionEvent(112, WS2, 'delete', 'session_server', 'done', NO_SUPERVISOR),
  actionEvent(113, WS2, 'delete', 'containers', 'started'),
  actionEvent(114, WS2, 'delete', 'containers', 'done', 'No container to remove.'),
  actionEvent(115, WS2, 'delete', 'broker_socket', 'started'),
  actionEvent(116, WS2, 'delete', 'broker_socket', 'done'),
  actionEvent(117, WS2, 'delete', 'files', 'started'),
  actionEvent(118, WS2, 'delete', 'files', 'done'),
  ev(119, 'workspace.gone', WS2, {}, 'Workspace deleted.'),
]

export const step = (status: StepView['status'], n: number, detail?: string): StepView =>
  ({ status, at: at(n), ...(detail !== undefined ? { detail } : {}) })

/** WS as GET /api/workspaces writes it after CLONE_OK. */
export function wsView(over: Partial<WorkspaceView> = {}): WorkspaceView {
  return {
    id: WS, repository_id: 1, full_name: 'krelinga/drydock', branch: 'main', state: 'running',
    state_detail: null, container_id: 'c0ffee0123456789', created_at: at(1),
    steps: {
      allocate: step('done', 3), clone: step('done', 6), resolve_config: step('done', 8), up: step('done', 12),
    },
    ...over,
  }
}

/** WS2 as GET /api/workspaces writes it after CLONE_FAILS_AT_UP. */
export function ws2View(over: Partial<WorkspaceView> = {}): WorkspaceView {
  return {
    id: WS2, repository_id: 2, full_name: 'krelinga/homelab', branch: 'main', state: 'failed',
    state_detail: 'The container start step failed.', container_id: null, created_at: at(20),
    steps: { up: step('failed', 24, 'The container start step failed.') },
    ...over,
  }
}

/** GET /api/workspaces: newest first, without `capacity` (as before §4.5 #17) unless given. */
export function listBody(...views: WorkspaceView[]): WorkspaceList {
  return { workspaces: views.length > 0 ? views : [ws2View(), wsView()] }
}

/** GET /api/workspaces with the cap, occupied counted as internal/workspace CapacityOf counts it. */
export function listWithCap(cap: number | null, ...views: WorkspaceView[]): WorkspaceList {
  const body = listBody(...views)
  const occupied = body.workspaces.filter((v) => ['pending', 'cloning', 'building', 'running'].includes(v.state)).length
  return { ...body, capacity: { cap, occupied } }
}

/** GET /api/workspaces/:id for WS, with the events it names, newest first. */
export function detailBody(over: Partial<WorkspaceView> = {}, events: StreamEvent[] = CLONE_OK): WorkspaceDetail {
  return { ...wsView(over), events: events.filter((e) => e.workspace_id === (over.id ?? WS)).slice().reverse() }
}

/** GET /api/repos with WS running on repo 1 and nothing on repo 2. */
export function catalogBody(over: Partial<CatalogView> = {}): CatalogView {
  return {
    refreshed_at: at(0),
    last_refresh_error: null,
    installations: [{ id: 101, account: 'krelinga', settings_url: 'https://github.com/settings/installations/101' }],
    repos: [
      {
        id: 1, installation_id: 101, full_name: 'krelinga/drydock', default_branch: 'main', private: true,
        archived: false, has_devcontainer: true, pushed_at: at(0), removed: false,
        workspace: { id: WS, state: 'running' },
      },
      {
        id: 2, installation_id: 101, full_name: 'krelinga/homelab', default_branch: 'main', private: true,
        archived: false, has_devcontainer: null, pushed_at: null, removed: false, workspace: null,
      },
    ],
    ...over,
  }
}
