// What ends each in-flight action (frontend §4.2), shared by every client
// (vscode-extension §5.2): the web UI's stores and the extension's commands
// begin their marks with these, so the two cannot disagree about when a press
// is over. Each action's end is one predicate over an `Outcome` (`OVER`),
// asked of each event (`settles*`) and, after a snapshot, of the entities
// (`overIn`), so the event path and the snapshot path cannot disagree either.
// The stores re-export them, so no web call site names this module.

import { WORKSPACE_STATES, type StreamEvent, type WorkspaceState } from './types'
import { deleteStuck, repoHeld, stopFailed, type Entities } from './reducer'

const MOVING_STATES: ReadonlySet<string> = new Set(['pending', 'cloning', 'building'])

/**
 * What one input says about how a workspace's action ended — from a stream
 * event (`eventOutcome`) or from the entities after a snapshot
 * (`entityOutcome`). Each action's "is it over?" is one predicate over this
 * (`OVER`), shared by its `settles*` function and its snapshot resolver, so
 * the event path and the snapshot path cannot disagree about what ends it.
 */
export interface Outcome {
  /** The workspace no longer exists. */
  gone: boolean
  /** The state the event moves it to, or the state the entities hold; null when the input says none. */
  state: WorkspaceState | null
  /** A delete stopped part-way and is annotated as such (`deleteStuck`). */
  stuckDelete: boolean
  /** A stop's sub-step failed, leaving the workspace running (`stopFailed`). */
  failedStop: boolean
  /** Stopped, waiting for a host-access approval (design §6): the event carries the request, or the entity holds one. */
  awaitingApproval: boolean
}

/** What the event says about workspace `id`'s action, or null when it says nothing. */
export function eventOutcome(id: string, ev: StreamEvent): Outcome | null {
  if (ev.workspace_id !== id) return null
  const none: Outcome = { gone: false, state: null, stuckDelete: false, failedStop: false, awaitingApproval: false }
  const d = ev.data ?? {}
  switch (ev.kind) {
    case 'workspace.gone':
      return { ...none, gone: true }
    case 'workspace.state': {
      const state = (WORKSPACE_STATES as readonly unknown[]).includes(d.state) ? d.state as WorkspaceState : null
      // Annotate, which writes a state event that moves nothing: a stuck
      // delete is deleting to deleting, and a failed stop running to
      // running, each with the sentence. The server writes nothing else of
      // that shape (an orphan's adoption carries no `from`), and both are
      // what the row keeps, so a snapshot reads the same end (`deleteStuck`,
      // `stopFailed`). Not the failed sub-step before it: a body read
      // between the two cannot yet tell a failed stop from one under way.
      const annotated = d.from === state && typeof d.detail === 'string' && d.detail !== ''
      return {
        ...none, state, stuckDelete: annotated && state === 'deleting', failedStop: annotated && state === 'running',
        awaitingApproval: d.approval !== undefined && d.approval !== null,
      }
    }
    default:
      return null
  }
}

/** What the entities say about workspace `id`'s action: as of the last snapshot, newer events included. */
export function entityOutcome(e: Entities, id: string): Outcome {
  const w = e.workspaces[id]
  // Only the workspace list, or `workspace.gone`, removes a workspace from
  // the entities (reducer.ts), so its absence is the server's word.
  if (w === undefined || e.gone[id] !== undefined) {
    return { gone: true, state: null, stuckDelete: false, failedStop: false, awaitingApproval: false }
  }
  return { gone: false, state: w.state, stuckDelete: deleteStuck(w), failedStop: stopFailed(w), awaitingApproval: w.approval !== null }
}

/**
 * Each action's end, over an Outcome. Not the move the server commits before
 * answering 202 — the move into `building` or `deleting`, the cleared
 * annotation — which is its receipt, not its end (§4.2).
 */
export const OVER = {
  /** A stop: `stopped`, a sub-step's failure (the workspace stays running), or a delete overtaking it. */
  stop: (o: Outcome) => o.gone || o.state === 'stopped' || o.state === 'deleting' || o.failedStop,
  /**
   * A start or a rebuild: leaving its build — for `running` or `failed`, or a
   * delete. Not the move *into* `building` or `cloning`, which the server
   * commits before answering, and not a step: a build is minutes of steps,
   * and the mark is what keeps a second tap from starting another.
   */
  build: (o: Outcome) => o.gone || (o.state !== null && !MOVING_STATES.has(o.state)),
  /** A delete: `workspace.gone`, or the annotation of one that stuck, after which Delete is the button again. */
  delete: (o: Outcome) => o.gone || o.stuckDelete,
  /**
   * Declining a host-access request: the request gone from the workspace —
   * the decline's own state event, or any move — or the workspace gone.
   */
  decline: (o: Outcome) => o.gone || (o.state !== null && !o.awaitingApproval),
  /** A session server (re)start, as far as the workspace can say: it left `running`, or went. */
  session: (o: Outcome) => o.gone || (o.state !== null && o.state !== 'running'),
} as const

/**
 * A job's kind, as the server's `workspace.job` names it
 * (internal/provision's Job* kinds) — one per button that starts a job.
 */
export type JobKind = 'create' | 'start' | 'rebuild' | 'approve' | 'stop' | 'delete' | 'supervisor'

/**
 * The end of the job a press started: a `workspace.job` of this kind, for
 * this workspace, newer than the mark (`since`, the stream position when it
 * was made). The server writes exactly one as every job ends — ok, failed or
 * cancelled — after the job's other events, so a press settles even on a
 * path whose events imply nothing (a restart whose stop failed, an end with
 * nothing new to say). It changes no entity: the card's state still comes
 * from the events before it, which the `OVER` predicates read.
 */
export function settlesJob(id: string, kind: JobKind, since: number): (ev: StreamEvent) => boolean {
  return (ev) => ev.kind === 'workspace.job' && ev.workspace_id === id && ev.id > since && ev.data?.kind === kind
}

export function settlesBy(id: string, over: (o: Outcome) => boolean): (ev: StreamEvent) => boolean {
  return (ev) => {
    const o = eventOutcome(id, ev)
    return o !== null && over(o)
  }
}

export function overIn(id: string, over: (o: Outcome) => boolean): (e: Entities) => boolean {
  return (e) => over(entityOutcome(e, id))
}

/**
 * What settles a clone: the create's own `workspace.state`, which carries the
 * repository id. The client has no workspace id until the 202's body names
 * one, and reading that body is exactly what §4.2 step 3 forbids — so the
 * clone is matched by the one id it did have. A create from another device
 * settles it too, which is right: the server answers ours `in_progress`.
 */
export function settlesClone(repoId: number): (ev: StreamEvent) => boolean {
  return (ev) => ev.kind === 'workspace.state' && ev.data?.repository_id === repoId
}

/** The same, asked of the entities: any workspace holds the repository (the create's row exists). */
export function cloneOverIn(repoId: number): (e: Entities) => boolean {
  return (e) => repoHeld(e, repoId)
}

/**
 * What settles a start: the workspace leaving its build, as a rebuild does —
 * never the move into `building` or `cloning`, which `restart()` commits
 * before it answers 202 (frontend §4.2: the event that ends the action).
 */
export function settlesStart(id: string): (ev: StreamEvent) => boolean {
  return settlesBy(id, OVER.build)
}

/**
 * What settles a stop: the move to `stopped`, or a sub-step's failure — the
 * workspace then stays `running` (design §6) and the button is Stop again. A
 * delete overtaking it ends it too.
 */
export function settlesStop(id: string): (ev: StreamEvent) => boolean {
  return settlesBy(id, OVER.stop)
}

/** What settles a rebuild: the workspace leaving its build (`OVER.build`). */
export function settlesRebuild(id: string): (ev: StreamEvent) => boolean {
  return settlesBy(id, OVER.build)
}

/**
 * What settles a delete: `workspace.gone`, or the annotation of a delete that
 * stuck — `deleting` to `deleting` with a detail naming the sub-step, after
 * which Delete is the button again. Not the move into `deleting`: that is
 * written before the 202, and nothing has been removed yet.
 */
export function settlesDelete(id: string): (ev: StreamEvent) => boolean {
  return settlesBy(id, OVER.delete)
}

/**
 * What settles a session server start or restart (POST …/supervisor): the
 * supervisor reporting any state but `exited` — a restart reports `exited`
 * first, as it stops the old server, and that is not the end of it — or the
 * workspace leaving `running`, which ends any session server with it.
 *
 * Only the second half has a snapshot form: the server writes nothing about
 * the supervisor before answering 202, so a body showing it `serving` cannot
 * say whether that is before the restart or after it. A session mark whose
 * settling event fell in a resync gap waits for the next supervisor.state.
 */
export function settlesSession(id: string): (ev: StreamEvent) => boolean {
  const workspaceOver = settlesBy(id, OVER.session)
  return (ev) => {
    if (workspaceOver(ev)) return true
    return ev.workspace_id === id && ev.kind === 'supervisor.state' && typeof ev.data?.state === 'string' &&
      ev.data.state !== 'exited'
  }
}

/**
 * What settles an approval (design §6): the start or rebuild it continues
 * leaving its build (`OVER.build`) — running, failed, or stopped for an
 * approval again — as a start's does. Not `config.approved` or the move into
 * `building`, which the server writes before it answers 202.
 */
export function settlesApprove(id: string): (ev: StreamEvent) => boolean {
  return settlesBy(id, OVER.build)
}

/** What settles a decline: the request gone from the workspace (`OVER.decline`). */
export function settlesDecline(id: string): (ev: StreamEvent) => boolean {
  return settlesBy(id, OVER.decline)
}

/**
 * A job action's whole settler: the end of the job it starts (`settlesJob`,
 * newer than `since`, the stream position when the mark was made) or the
 * event its own predicate accepts, whichever comes first.
 */
export function settlesJobOr(id: string, kind: JobKind, since: number, settles: (ev: StreamEvent) => boolean): (ev: StreamEvent) => boolean {
  const ended = settlesJob(id, kind, since)
  return (ev) => ended(ev) || settles(ev)
}

/** What settles a catalog refresh (POST /api/repos/refresh): its own outcome event. */
export function settlesRefresh(ev: StreamEvent): boolean {
  return ev.kind === 'repo.refreshed' || ev.kind === 'repo.refresh_failed'
}

/**
 * The events that end a requested identity check (*Check now*): the verdict
 * changed or a failure cleared, the check failed, or — the common, healthy
 * case — it found nothing to announce. The caller also requires the event to
 * be newer than the press.
 */
export const CHECK_SETTLERS: readonly string[] = ['auth.identity', 'auth.identity_check_failed', 'auth.identity_checked']
export function settlesCheck(ev: StreamEvent): boolean {
  return CHECK_SETTLERS.includes(ev.kind)
}
