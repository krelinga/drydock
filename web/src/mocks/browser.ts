// `npm run dev:mock` — the UI with no backend at all (frontend §10). Loaded
// only when Vite's mode is `mock`, so none of this reaches dist/.
//
// The console gets a handle for the states that are tedious to produce for
// real: `drydockMock.expire()` lapses the session (the next request is a 401),
// `drydockMock.lockout()` and `drydockMock.notConfigured()` set up the
// sign-in failures. The password is `drydock`.

import { setupWorker } from 'msw/browser'
import { handlersFor, MOCK_PASSWORD, newBackend } from './backend'

export async function startMockWorker(): Promise<void> {
  const backend = newBackend()
  const worker = setupWorker(...handlersFor(backend))
  await worker.start({ onUnhandledFrame: 'bypass', quiet: true })

  const handle = {
    password: MOCK_PASSWORD,
    backend,
    expire() { backend.signedIn = false },
    lockout(seconds = 120) { backend.lockedUntil = Date.now() + seconds * 1000 },
    notConfigured(on = true) { backend.notConfigured = on },
  }
  ;(window as unknown as { drydockMock: typeof handle }).drydockMock = handle
  console.info('[drydock] mock API active. Password: %s. See window.drydockMock.', MOCK_PASSWORD)
}
