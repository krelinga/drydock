import { createPinia, type Pinia } from 'pinia'
import type { Router, RouterHistory } from 'vue-router'
import { createAppRouter } from './routes'
import { installAuth } from './auth'

/**
 * Everything but the DOM: one store, one router, the 401 path wired between
 * them. main.ts mounts it with browser history; the specs mount the same thing
 * with memory history, so what they test is what ships.
 */
export function createDrydock(history: RouterHistory): { pinia: Pinia; router: Router } {
  const pinia = createPinia()
  const router = createAppRouter(history)
  installAuth(router, pinia)
  return { pinia, router }
}
