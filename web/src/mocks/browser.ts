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
//   drydockMock.clone(3, 'up')    the same, failing at the `up` step
//   drydockMock.gone(id)          delete a workspace
//   drydockMock.dropStream()      cut the stream; the browser retries (CONNECTING)
//   drydockMock.refreshFails()    the next catalog refresh reports a failure
//   drydockMock.noApp()           GET /api/repos answers app_not_configured

import { setupWorker } from 'msw/browser'
import { cloneScript, completeRefresh, emit, handlersFor, MOCK_PASSWORD, newBackend } from './backend'

export async function startMockWorker(): Promise<void> {
  const backend = newBackend()
  const worker = setupWorker(...handlersFor(backend))
  await worker.start({ onUnhandledFrame: 'bypass', quiet: true })

  let n = 0
  const handle = {
    password: MOCK_PASSWORD,
    backend,
    expire() { backend.signedIn = false },
    lockout(seconds = 120) { backend.lockedUntil = Date.now() + seconds * 1000 },
    notConfigured(on = true) { backend.notConfigured = on },
    clone(repositoryId: number, failAt?: string, intervalMs = 1000) {
      const id = `01JB${String(Date.now()).padStart(18, '0')}${String(n++).padStart(4, '0')}`.slice(0, 26)
      const script = cloneScript(backend, repositoryId, id, failAt)
      script.forEach((play, i) => setTimeout(play, i * intervalMs))
      return id
    },
    gone(id: string) {
      emit(backend, 'workspace.state', { workspace_id: id, data: { state: 'deleting', from: backend.workspaces[id]?.state } })
      setTimeout(() => emit(backend, 'workspace.gone', { workspace_id: id, data: {} }), 800)
    },
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
  }
  ;(window as unknown as { drydockMock: typeof handle }).drydockMock = handle
  console.info('[drydock] mock API active. Password: %s. See window.drydockMock.', MOCK_PASSWORD)
}
