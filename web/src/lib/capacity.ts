// The concurrent-container cap, as the home screen shows it (design §1,
// frontend §4.5 #17, §9).
//
// The cap comes from GET /api/workspaces; the occupied count is *counted*,
// here, from the workspace entities the reducer keeps — never stored, and
// never taken from an event of its own. Every move that changes the count is
// a `workspace.state` event the reducer already applies, so counting the
// entities is live for free, and it cannot disagree with the list it was
// loaded from: the server counts that list's own rows by the same rule
// (internal/workspace CapacityOf). The risk in counting is the rule drifting
// from the server's, so OCCUPYING is checked against `workspace.Occupying`'s
// source by capacity.spec.ts, and against the server's own count of a
// snapshot by the mock backend's.

import type { WorkspaceState } from '../api/types'
import type { Entities, Workspace } from '../stores/reducer'

/**
 * The states that hold, or are building, a container slot: Go's
 * `workspace.Occupying` (internal/workspace/state.go), which is what create,
 * start and rebuild are refused against.
 */
export const OCCUPYING: ReadonlySet<WorkspaceState> = new Set<WorkspaceState>(['pending', 'cloning', 'building', 'running'])

export interface Capacity {
  /** Null while unknown: before the list is loaded, or with no cap. */
  cap: number | null
  occupied: number
  /** At or over the cap: a create, a start or a rebuild from a stopped or failed workspace will be refused. */
  full: boolean
}

export function capacity(e: Entities): Capacity {
  let occupied = 0
  for (const w of Object.values(e.workspaces)) {
    if (w.state !== null && OCCUPYING.has(w.state)) occupied++
  }
  return { cap: e.cap, occupied, full: e.cap !== null && occupied >= e.cap }
}

/**
 * Whether an action takes a slot it does not already hold: a clone always; a
 * start, from stopped or failed; a rebuild of a workspace that is not
 * running (a running one keeps its own). Stop and delete free one.
 */
export function needsSlot(action: string | null, w: Workspace | null): boolean {
  switch (action) {
    case 'clone':
      return true
    case 'start':
      return true
    case 'rebuild':
      return w === null || w.state !== 'running'
    default:
      return false
  }
}
