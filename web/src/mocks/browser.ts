// `npm run dev:mock` — the UI with no backend at all (frontend §10). Loaded
// only when Vite's mode is `mock`, so none of this reaches dist/.
//
// The console gets a handle for the states that are tedious to produce for
// real. The password is `drydock`.
//
//   drydockMock.expire()          lapse the session (the next request is a 401)
//   drydockMock.lockout()         lock sign-in for two minutes
//   drydockMock.notConfigured()   no password set
//   drydockMock.clone(3)          play a clone of repo 3 on the stream, ~1 event/s
//                                 (as another device would: no click involved)
//   drydockMock.clone(3, 'up')    the same, failing at the `up` step
//   drydockMock.failNext('up')    the next Clone, Start or Rebuild fails at that step
//   drydockMock.failAction('files')  the next Stop or Delete fails at that sub-step:
//                                 a stop leaves the workspace running; a delete
//                                 sticks in deleting, and Delete again resumes it
//                                 (sub-steps: session_server, container, containers,
//                                 broker_socket, files)
//   drydockMock.capacity(1)       set the cap; the next Clone over it is at_capacity
//   drydockMock.elsewhere(3)      another device created a workspace for repo 3 and
//                                 its event has not arrived: the next Clone of repo 3
//                                 is answered in_progress
//   drydockMock.stop(id)          another device stops a running workspace
//   drydockMock.slow(ms)          the gap between a script's events (default 900)
//   drydockMock.gone(id)          another device deletes a workspace (or resumes a stuck delete)
//   drydockMock.dropStream()      cut the stream; the browser retries (CONNECTING)
//   drydockMock.refreshFails()    the next catalog refresh reports a failure
//   drydockMock.noApp()           GET /api/repos answers app_not_configured
//
// Secrets (Phase 4). The form checks everything the server does before it
// sends, so a server-side refusal is reached by forcing one:
//
//   drydockMock.refuseSecret('secret_name_reserved')   the next secret write is
//                                 refused with that code (any §10.1 code, with the
//                                 server's own detail), whatever it sent
//   drydockMock.noSecretsKey()    every /api/secrets route answers secrets_not_configured
//   drydockMock.undeliverable()   the key no longer opens STRIPE_TEST_KEY: the fleet
//                                 banner, from the event and from a reload's GET.
//                                 Storing its value again (Edit…) or deleting it
//                                 repairs it, and secret.deliverable clears the banner
//   drydockMock.undeliverable('NAME', 'breaks_write_rules')   another row, another
//                                 reason (deleting it is that one's repair)
//   drydockMock.fetchSecrets(id)  a workspace fetches its secrets: last access moves,
//                                 with no event, so only a refetch shows it
//   drydockMock.needsRestart(id)  a rotation reports that workspace as
//                                 needs_supervisor_restart (Phase 5's hook)
//   drydockMock.rotateElsewhere(name)  another device rotates a secret
//
// The shared Claude login (Phase 5, design §7.3):
//
//   drydockMock.identity('blanked')   the watch stores a new verdict and says so:
//                                 ok | expiring | expired | blanked | absent. The
//                                 fleet banner and every running card follow
//   drydockMock.identity('expiring', 36 * 3600e3)   expiring, 36 hours out
//   drydockMock.identityCheckFails()  a check could not read the volume; the
//                                 stored state stands and Settings says why

import { setupWorker } from 'msw/browser'
import type { IdentityState } from '../api/types'
import {
  cloneScript, completeRefresh, emit, failIdentityCheck, handlersFor, identityView, MOCK_PASSWORD, newBackend,
  nextWorkspaceId, recordSecretFetch, scheduleDelete, scheduleStop, secretMeta, secretUndeliverable, setIdentity,
} from './backend'

/** The server's detail for each refusal dev:mock can force: what internal/secrets would say. */
const REFUSALS: Record<string, { status: number; message: string; detail?: string }> = {
  secrets_not_configured: { status: 503, message: 'No secrets master key is configured, so secrets cannot be stored.', detail: 'Start drydock serve with --secrets-key; the installer creates the key.' },
  secret_name_invalid: { status: 400, message: "A secret's name is its environment variable name.", detail: 'Use capital letters, digits and underscores, not starting with a digit, at most 128 characters.' },
  secret_name_reserved: { status: 400, message: 'GH_TOKEN is reserved.', detail: "Drydock refuses it because gh reads GH_* itself; GH_TOKEN would shadow the shim's repository-scoped token (§9.2)." },
  secret_value_empty: { status: 400, message: 'A secret needs a value.', detail: 'An empty value cannot be told apart from an unset variable.' },
  secret_value_control_character: { status: 400, message: "A secret's value must be a single line with no control characters.", detail: 'It contains a newline (U+000A) at byte 7. A multi-line credential, such as a PEM, goes in as base64.' },
  secret_value_too_long: { status: 400, message: 'That value is too long.', detail: 'A value is at most 32768 bytes. Encode a large or multi-line credential, such as a PEM, as base64.' },
  secret_value_required: { status: 400, message: 'A new secret needs a value.', detail: 'There is no secret by this name, so there is no stored value to keep.' },
  secret_reach_required: { status: 400, message: 'Say what someone could do with this secret.', detail: 'The reach field is required: it is the decision to grant, written down.' },
  secret_reach_too_long: { status: 400, message: 'The reach is too long.', detail: 'At most 2000 bytes.' },
  secret_description_too_long: { status: 400, message: 'The description is too long.', detail: 'At most 4000 bytes.' },
  secret_description_invalid: { status: 400, message: 'The description must be text.', detail: 'It is not valid UTF-8.' },
  unknown_repository: { status: 400, message: 'That repository is not in the catalog.', detail: 'No repository has id 99. Refresh the repository list and try again.' },
  not_found: { status: 404, message: 'There is no secret by that name.' },
  bad_request: { status: 400, message: 'Send a JSON object with the documented fields.' },
}

export async function startMockWorker(): Promise<void> {
  const backend = newBackend({ supervisor: true })
  const worker = setupWorker(...handlersFor(backend))
  await worker.start({ onUnhandledFrame: 'bypass', quiet: true })

  const handle = {
    password: MOCK_PASSWORD,
    backend,
    expire() { backend.signedIn = false },
    lockout(seconds = 120) { backend.lockedUntil = Date.now() + seconds * 1000 },
    notConfigured(on = true) { backend.notConfigured = on },
    clone(repositoryId: number, failAt?: string, intervalMs = 1000) {
      const id = nextWorkspaceId(backend)
      const script = cloneScript(backend, repositoryId, id, failAt)
      script.forEach((play, i) => setTimeout(() => play(), i * intervalMs))
      return id
    },
    failNext(step: string) { backend.failNext = step },
    failAction(step: string) { backend.failAction = step },
    capacity(n: number) { backend.capacity = n },
    elsewhere(repositoryId: number) {
      // A row with no event: what a create on another device looks like in
      // the instant before its event reaches this one.
      const id = nextWorkspaceId(backend)
      backend.workspaces[id] = {
        id, repository_id: repositoryId, branch: 'main', state: 'pending', state_detail: null,
        container_id: null, created_at: new Date().toISOString(), steps: {},
      }
      return id
    },
    stop(id: string) { scheduleStop(backend, id) },
    slow(ms: number) { backend.scriptIntervalMs = ms },
    gone(id: string) { scheduleDelete(backend, id) },
    dropStream() { for (const s of backend.streams) s.error() },
    refreshFails() {
      backend.refreshMode = 'manual'
      const poll = setInterval(() => {
        if (backend.pendingRefreshes === 0) return
        clearInterval(poll)
        setTimeout(() => {
          completeRefresh(backend, false)
          backend.refreshMode = 'auto'
        }, backend.refreshDelayMs)
      }, 100)
    },
    noApp(on = true) { backend.appConfigured = !on },
    refuseSecret(code: string) {
      const r = REFUSALS[code]
      if (r === undefined) throw new Error(`no such refusal; one of ${Object.keys(REFUSALS).join(', ')}`)
      backend.refuseNextSecret = { code, ...r }
    },
    noSecretsKey(on = true) { backend.secretsKey = !on },
    undeliverable(name?: string, reason = 'does_not_open') {
      return secretUndeliverable(backend, name === undefined ? undefined : [{ name, reason }])
    },
    fetchSecrets(id: string) { return recordSecretFetch(backend, id) },
    needsRestart(id: string) { backend.staleRestart.push(id) },
    identity(state: IdentityState, expiresInMs?: number) { return setIdentity(backend, identityView(state, expiresInMs)) },
    identityCheckFails() { return failIdentityCheck(backend) },
    rotateElsewhere(name: string) {
      const s = backend.secrets[name]
      if (s === undefined) return
      s.value = `${s.value}x`
      s.rotated_at = new Date().toISOString()
      emit(backend, 'secret.rotated', { message: `Rotated the secret ${name}.`, data: { secret: secretMeta(backend, s), stale: { new_commands: [], needs_supervisor_restart: [] } } })
    },
  }
  ;(window as unknown as { drydockMock: typeof handle }).drydockMock = handle
  console.info('[drydock] mock API active. Password: %s. See window.drydockMock.', MOCK_PASSWORD)
}
