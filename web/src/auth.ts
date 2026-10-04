// The 401 path (frontend §2.2, §4.4), and the boot probe.
//
// Authentication state is derived from traffic, not stored: the app learns it
// is signed in when GET /api/auth/session answers 200, and learns it is signed
// out from any 401 on any request. Signing out — by 401 or by the button —
// clears every store's entity state before routing to /signin, because the one
// thing the UI must never show after the gap is a confident rendering of a
// world that may have moved on.

import type { Pinia } from 'pinia'
import type { Router } from 'vue-router'
import { ApiError, onUnauthorized } from './api/client'
import { clearEntityState } from './stores/registry'
import { useSessionStore } from './stores/session'
import { signInLocation } from './routes'

export function installAuth(router: Router, pinia: Pinia): void {
  const session = useSessionStore(pinia)

  onUnauthorized(() => {
    const wasSignedIn = session.status === 'signed-in'
    clearEntityState(pinia)
    session.status = 'signed-out'
    const here = router.currentRoute.value
    // During the boot probe there is no "here" yet — the guard below is
    // mid-navigation and redirects with the right `return` itself. On
    // /signin a 401 is a bad password, and there is nowhere to go.
    if (wasSignedIn && here.meta.public !== true) {
      void router.replace(signInLocation(here.fullPath))
    }
  })

  router.beforeEach(async (to) => {
    if (to.meta.public === true) return true
    if (session.status === 'unknown') {
      try {
        await session.load()
        session.probeError = null
      } catch (e) {
        // A 401 has already marked us signed out. Anything else is "could not
        // tell": let the navigation land and let the shell say so, rather than
        // sending someone with a working session to a password prompt.
        if (!(e instanceof ApiError) || e.status !== 401) {
          session.probeError = e instanceof ApiError ? e : new ApiError(0, 'network')
          return true
        }
      }
    }
    if (session.status !== 'signed-in') return signInLocation(to.fullPath)
    return true
  })

  router.afterEach((to) => {
    document.title = to.meta.title ? `${to.meta.title} · Drydock` : 'Drydock'
  })
}
