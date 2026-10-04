// The workspace card's one status line and one primary action (frontend
// §6.1), as a pure function of the entities, so the state table is a
// parameterized test rather than something read off a rendered card.
//
// §6.1's table is a join of two state machines: `workspace.state` and
// `supervisor.state`. Phase 2 has only the first. The supervisor half is a
// seam, not a guess — see `supervisorHalf` — and until it is filled a
// running workspace says exactly what is known, that its container is up.

import { currentStep, failedStep, type Workspace } from '../stores/reducer'

export type Tone = 'ok' | 'bad' | 'busy' | 'idle'

/** The one action a card offers. Phase 2 builds `start`; the others are named for the seam. */
export type CardAction = 'start' | null

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
 * claim, not a reading.
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
    case 'running':
      return supervisorHalf(w) ?? { line: label, tone: 'ok', note: w.detail, action: null }
    case 'stopped':
      // Fig 3: stopped says what survived, which is what makes Start cheap.
      return { line: label, tone: 'idle', note: w.detail ?? 'The clone is intact.', action: 'start' }
    case 'failed': {
      // §6.1: `failed` names the step, never "failed" alone. Rebuild is the
      // design's action here (Phase 6); start is what Phase 2 has, and the
      // server accepts it from failed.
      const step = failedStep(w)
      const line = step !== null ? `Failed while ${STEP_LABEL[step] ?? step}` : label
      return { line, tone: 'bad', note: w.detail, action: 'start' }
    }
    case 'deleting':
      return { line: label, tone: 'idle', note: null, action: null }
    default:
      return { line: 'Unknown', tone: 'idle', note: null, action: null }
  }
}
