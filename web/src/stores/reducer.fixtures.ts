// Recorded event sequences for the reducer's specs (frontend §10): what the
// server writes, frame for frame, for the flows Phase 2 has. Each frame's
// `data` is exactly what the Go Emit call marshals — internal/workspace
// (Create, Move, stepEvent, Remove, Adopt), internal/reconcile,
// internal/catalog and internal/broker — so a reducer test over these is a
// test against the server's shapes, not against our memory of them.

import type { CatalogView, StreamEvent } from '../api/types'

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
  ev(13, 'workspace.state', WS, { state: 'running', from: 'building' }, 'Running.'),
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

/** GET /api/repos with WS running on repo 1 and nothing on repo 2. */
export function catalogBody(over: Partial<CatalogView> = {}): CatalogView {
  return {
    refreshed_at: at(0),
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
