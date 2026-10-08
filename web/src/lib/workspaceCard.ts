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
export type CardAction =
  | 'start' | 'stop' | 'rebuild' | 'delete'
  // The supervisor half's (Phase 5): start or restart the session server
  // (POST …/supervisor), and open the environment in Claude — a link, the
  // one action that is not a mutation.
  | 'start_session' | 'restart_session' | 'open'
  // Design §6: a stopped workspace waiting for the operator to approve its
  // configuration's host access. Rendered as the request itself — what it
  // asks for, the warning, Approve and continue and Cancel — never as a
  // bare button, because approving unread is the failure it exists to stop.
  | 'approve'
  | null

/**
 * The fleet's Claude login as the identity watch stored it (design §7.3), or
 * null when it is not known. It overrides the session half of every card
 * (§6.6): one cause, one message, one button — and that button is the
 * banner's, never a card's.
 */
export type FleetLogin = 'ok' | 'expiring' | 'expired' | 'blanked' | 'absent' | null

/**
 * The logins under which no session server can run until someone signs in.
 * Not expired: that dates the access token, which a starting server renews
 * from the live refresh token beside it (design §7.3), and the supervisor
 * starts servers under it — the server's own row speaks.
 */
const SIGNED_OUT = new Set<FleetLogin>(['blanked', 'absent'])

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
  /** For `open`: the environment's link (design §8), built from its id. */
  link?: string | null
  /** For the waiting row: when the wait began, for its elapsed time. */
  since?: string | null
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
 * Reasons that are the container's or Drydock's fault, not the login's: the
 * fleet override leaves these cards alone (§6.6 keeps the override narrow),
 * and each points at the fix that can work.
 */
const CONFIG_FAULTS = new Set(['not_trusted', 'hang_remote_dialog', 'hang_trust', 'stale_broker_mount'])

/**
 * A restart (or a stop) whose stop half failed (design §8, internal/supervisor
 * Stop). `survived_kill`: the server outlived SIGKILL, so asking again cannot
 * help and replacing the container is the fix — the container's fault, not
 * the login's, so the fleet override leaves it too. `stop_failed`: Docker
 * could not be asked; asking again is the fix once it answers, and under a
 * signed-out fleet it waits behind the banner like any restart.
 */
const SURVIVED_KILL = 'survived_kill'
const STOP_FAILED = 'stop_failed'

/**
 * §6.1's rows for `running` × `supervisor.state`, read from the supervisor
 * entity — `supervisor.state` events and the views' `supervisor`, never
 * `workspace.state` — with §6.6's fleet override over them. Its action takes
 * the card; Stop is the detail view's.
 */
function supervisorHalf(w: Workspace, fleet: FleetLogin): CardStatus | null {
  // A server that does not report a supervisor (older than Phase 5) has said
  // nothing about a session: the workspace half speaks, as it did then.
  if (!w.supervisorKnown) return null
  const s = w.supervisor
  const configFault = s !== null && s.state === 'degraded' &&
    (CONFIG_FAULTS.has(s.reason ?? '') || s.reason === SURVIVED_KILL)
  // §6.6: a signed-out fleet replaces the session half of every running
  // card — no session line, no session button; the banner holds the one Sign
  // in to Claude — except where the card's own fault is not the login's. The
  // card says only what is still its own, that the container runs; the
  // "Waiting on Claude sign-in." under it is WorkspaceIdentityNote's
  // (lib/identity.ts cardOverlay), so the sentence has one source and
  // appears once.
  if (SIGNED_OUT.has(fleet) && !configFault && (s === null || s.state !== 'degraded' || s.reason !== 'bad_command_line')) {
    return { line: 'Running', tone: 'idle', note: null, action: null }
  }
  if (s === null) {
    return { line: 'Container up, no session', tone: 'idle', note: null, action: 'start_session' }
  }
  switch (s.state) {
    case 'starting':
      return { line: 'Starting session…', tone: 'busy', note: s.reason === 'backoff' ? s.detail : null, action: null }
    case 'awaiting_login':
      // The fix is fleet-wide; the banner has its one button.
      return { line: 'Claude is not signed in', tone: 'bad', note: s.detail, action: null }
    case 'waiting_registration':
      // Not a failure (§8): it retries on its own, and pressing anything is
      // the mistake. Elapsed time, no action, never the word failed.
      return {
        line: 'Waiting for the previous session server to release the folder', tone: 'busy',
        note: 'This clears on its own; nothing to press.', action: null, since: s.since,
      }
    case 'serving': {
      const sess = w.session
      const line = sess !== null && sess.capacityUsed !== null && sess.capacityTotal !== null
        ? `Capacity ${sess.capacityUsed} / ${sess.capacityTotal}`
        : 'Serving'
      const link = sess?.url ?? null
      return { line, tone: 'ok', note: null, action: link !== null ? 'open' : null, link }
    }
    case 'degraded':
      if (s.reason === SURVIVED_KILL) {
        return { line: 'Session server would not stop', tone: 'bad', note: s.detail, action: 'rebuild' }
      }
      if (s.reason === STOP_FAILED) {
        return { line: 'Session server did not stop', tone: 'bad', note: s.detail, action: 'restart_session' }
      }
      if (configFault) {
        // §9: the trust record or the consent key is missing — the Feature
        // writes both at create, so a rebuild is the fix, not a restart.
        return { line: 'Container misconfigured', tone: 'bad', note: s.detail, action: 'rebuild' }
      }
      if (s.reason === 'bad_command_line') {
        return { line: 'Drydock built a bad command line', tone: 'bad', note: s.detail, action: null }
      }
      return { line: 'Session degraded', tone: 'bad', note: s.detail, action: 'restart_session' }
    case 'exited':
      return { line: 'Session stopped', tone: 'idle', note: null, action: 'start_session' }
  }
}

export function cardStatus(w: Workspace, fleet: FleetLogin = null): CardStatus {
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
      return supervisorHalf(w, fleet) ?? { line: label, tone: 'ok', note: w.detail, action: 'stop' }
    }
    case 'stopped':
      // Design §6: the run stopped before anything reached the host, and
      // waits for the operator. Not a failure, so not the failure tone's
      // word, but the one card that must be read before anything is pressed.
      if (w.approval !== null) {
        return { line: 'Needs approval', tone: 'bad', note: 'This configuration asks for host access.', action: 'approve' }
      }
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
  if (w.state !== 'running') return false
  const a = cardStatus(w).action
  // A running workspace is stoppable unless a stop is already under way; a
  // failed stop's card offers Stop again.
  return a === 'stop' || liveAction(w) === null
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
