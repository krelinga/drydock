// Every store that holds entity state, so signing out can clear all of them
// (frontend §4.4). A store added in a later phase is added here; the 401 test
// asserts the list is honoured, not what is on it.
//
// Stores are options stores so `$reset` exists on each: a setup store has no
// generic reset, and a hand-written one is a reset that forgets the field
// added next month.

import type { Pinia } from 'pinia'
import { useSessionStore } from './session'

const ENTITY_STORES = [useSessionStore] as const

export function clearEntityState(pinia?: Pinia): void {
  for (const use of ENTITY_STORES) use(pinia).$reset()
}
