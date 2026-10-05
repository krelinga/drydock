// The workspace card's one status line and one primary action (frontend
// §6.1), as a pure function of the entities, so the state table is a
// parameterized test rather than something read off a rendered card.
//
// §6.1's table is a join of two state machines: `workspace.state` and
// `supervisor.state`. Phase 2 has only the first. The supervisor half is a
// seam, not a guess — see `supervisorHalf` — and until it is filled a
// running workspace says exactly what is known, that its container is up.
//
// Phase 6 adds a third input, the stop or delete in progress
// (`liveAction`), because both run while the state stands still: a stop
// leaves the workspace `running` until its last sub-step, and a failed one
// leaves it `running` for good. A card that read the state alone would offer
// Stop on a workspace already stopping.

import { currentStep, deleteStuck, failedStep, liveAction, stopFailed, type Repo, type Workspace } from '../stores/reducer'
import { needsSlot } from './capacity'

export type Tone = 'ok' | 'bad' | 'busy' | 'idle'

/**
 * The one action a card offers. `delete` is only ever the resume of a delete
 * that stuck — the first delete is the detail view's, behind its confirm
 * (§6.5), and never a card's primary action.
 */
export type CardAction = 'start' | 'stop' | 'rebuild' | 'delete' | null

/**
 * What is rendered where an action would be: the action, or — when it needs a
 * container slot and Drydock is at its cap — `make_room`, a pointer to the
 * workspaces under Running that can be stopped (design §1). Never the action
 * disabled: one that cannot work is replaced by the one that can (§6.6).
 */
export type ShownAction = CardAction | 'clone' | 'make_room'

/** `action` at the cap: replaced by `make_room` when it would need a slot. */
export function withRoom(action: CardAction | 'clone', w: Workspace | null, full: boolean): ShownAction {
  return full && needsSlot(action, w) ? 'make_room' : action
}

export interface CardStatus {
  /** One line. */
  line: string
  tone: Tone
  /** The sentence under it, when there is one. */
  note: string | null
  action: CardAction
}

const STATE_LABEL: Record<string, string> = {
  pending: 'Waiting to start',
  cloning: 'Cloning',
  building: 'Building',
  running: 'Running',
  stopped: 'Stopped',
  failed: 'Failed',
  deleting: 'Deleting…',
}

/** Design §6's steps as the card says them: "Failed while <this>", "Building · <this>…". */
export const STEP_LABEL: Record<string, string> = {
  allocate: 'allocating', clone: 'cloning', resolve_config: 'resolving config',
  credential_volume: 'preparing credentials', broker_socket: 'opening the broker socket',
  up: 'starting the container', verify: 'verifying', session_server: 'starting the session server',
}

/** A stop's or a delete's sub-steps as the card says them: "Stopping · stopping the container…". */
export const ACTION_STEP_LABEL: Record<string, string> = {
  session_server: 'stopping the session server', container: 'stopping the container',
  containers: 'removing the containers', broker_socket: 'closing GitHub access', files: 'removing the clone',
}

/** A sub-step's name for a heading: "Removing the clone". */
export function actionStepTitle(name: string): string {
  const s = ACTION_STEP_LABEL[name] ?? name
  return s.charAt(0).toUpperCase() + s.slice(1)
}

/** The step's name for a heading: "Starting the container". */
export function stepTitle(name: string): string {
  const s = STEP_LABEL[name] ?? name
  return s.charAt(0).toUpperCase() + s.slice(1)
}

const MOVING = new Set(['pending', 'cloning', 'building'])

/**
 * SEAM (Phase 5): §6.1's rows for `running` × `supervisor.state` — absent,
 * starting, awaiting_login, waiting_registration, serving, degraded, exited —
 * and §6.6's fleet override belong here, fed by a supervisor entity the
 * reducer does not have yet. Until it does, return null and let the
 * workspace half speak. Do not fill this from `workspace.state` alone: a
 * `running` container says nothing about whether a session server is serving,
 * and "Container up, no session" with a Start session button would be a
 * claim, not a reading. When it fills, its action takes the card, Stop moves
 * to the detail view's Actions beside Rebuild, and §6.5 has it confirm when
 * sessions are live.
 */
function supervisorHalf(_w: Workspace): CardStatus | null {
  return null
}

export function cardStatus(w: Workspace): CardStatus {
  const label = STATE_LABEL[w.state ?? ''] ?? 'Unknown'
  switch (w.state) {
    case 'pending':
    case 'cloning':
    case 'building': {
      const s = currentStep(w)
      const line = s !== null && s.status === 'started' && MOVING.has(w.state)
        ? `${label} · ${STEP_LABEL[s.name] ?? s.name}…`
        : label
      return { line, tone: 'busy', note: w.detail, action: null }
    }
    case 'running': {
      const run = liveAction(w)
      if (run !== null) {
        const what = ACTION_STEP_LABEL[run.last.name] ?? run.last.name
        // A stop that failed leaves the workspace running (design §6), and
        // asking again is the one thing to do about it.
        if (run.last.status === 'failed') {
          return { line: `Stop failed while ${what}`, tone: 'bad', note: run.last.detail, action: 'stop' }
        }
        // Stopping: nothing to press. A second Stop is a 409, and Start
        // cannot work until the stop lands.
        return { line: `Stopping · ${what}…`, tone: 'busy', note: null, action: null }
      }
      // A stop that failed, after the fact: the server annotated the row, so
      // this reads the same from a reload of the list as it did live (§4.5
      // #15). The sentence is the server's; the sub-step is structure.
      if (stopFailed(w)) {
        const step = w.lastAction!.step
        return { line: `Stop failed while ${ACTION_STEP_LABEL[step] ?? step}`, tone: 'bad', note: w.detail, action: 'stop' }
      }
      return supervisorHalf(w) ?? { line: label, tone: 'ok', note: w.detail, action: 'stop' }
    }
    case 'stopped':
      // Fig 3: stopped says what survived, which is what makes Start cheap.
      return { line: label, tone: 'idle', note: w.detail ?? 'The clone is intact.', action: 'start' }
    case 'failed': {
      // §6.1: `failed` names the step, never "failed" alone, and offers
      // Rebuild: a new container from the same clone. (A start from failed
      // also replaces the container now, design §5; Rebuild says so.)
      const step = failedStep(w)
      const line = step !== null ? `Failed while ${STEP_LABEL[step] ?? step}` : label
      return { line, tone: 'bad', note: w.detail, action: 'rebuild' }
    }
    case 'deleting': {
      const run = liveAction(w)
      if (run !== null) {
        const what = ACTION_STEP_LABEL[run.last.name] ?? run.last.name
        return { line: `Deleting · ${what}…`, tone: 'busy', note: null, action: null }
      }
      // A delete that stuck stays deleting, its annotation naming the
      // sub-step, and Delete is still the button: asking again resumes it.
      // The full name was typed when it began; the persisted state is the
      // record of that (internal/provision ResumeDelete).
      if (deleteStuck(w)) {
        return { line: 'Delete stopped part-way', tone: 'bad', note: w.detail, action: w.fullName !== null ? 'delete' : null }
      }
      return { line: label, tone: 'idle', note: null, action: null }
    }
    default:
      return { line: 'Unknown', tone: 'idle', note: null, action: null }
  }
}

/** The one action a catalog row offers. */
export type RowAction = 'clone' | CardAction

/**
 * Whether a workspace can be stopped from its card: what the Running section
 * sorts first, so at the cap the cards to stop are the first ones read
 * (design §1: "the UI shows you which one to stop").
 */
export function stoppable(w: Workspace): boolean {
  return cardStatus(w).action === 'stop'
}

/**
 * A catalog row's action (§6.1: one per row). Clone only where no workspace
 * holds the repository at all: the server answers a create `409 in_progress`
 * while the repository has a workspace in *any* state — a failed or stopped
 * one still holds the clone and comes back by Start, and a deleting one is
 * still removing it (design §5). So a held repository offers its workspace's
 * own action, and a removed one is read-only (a create would be `404`).
 */
export function rowAction(row: { repo: Repo; workspace: Workspace | null; held: boolean }): RowAction {
  if (row.workspace !== null) return cardStatus(row.workspace).action
  if (row.held || row.repo.removed) return null
  return 'clone'
}
