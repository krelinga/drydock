import type { Router } from 'vue-router'
import { clearEntityState } from './stores/registry'
import { useSessionStore } from './stores/session'

/**
 * After a successful sign-out: the same clearing the 401 path does (auth.ts),
 * then /signin with no `return` — the operator chose to leave.
 */
export async function endSession(router: Router): Promise<void> {
  clearEntityState()
  useSessionStore().status = 'signed-out'
  await router.replace({ name: 'signin' })
}
