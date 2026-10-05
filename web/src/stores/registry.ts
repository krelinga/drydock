// Every store that holds entity state, so signing out can clear all of them
// (frontend §4.4). A store added in a later phase is added here; the 401 test
// asserts the list is honoured, not what is on it.
//
// Stores are options stores so `$reset` exists on each: a setup store has no
// generic reset, and a hand-written one is a reset that forgets the field
// added next month.
//
// The stream is closed before anything is reset: an EventSource left open
// would keep feeding the reducer after the entities were cleared, which is
// the stale world §4.4 exists to prevent, delivered one event at a time.

import type { Pinia } from 'pinia'
import { useCatalogStore } from './catalog'
import { useSessionStore } from './session'
import { useStreamStore } from './stream'
import { useSecretsStore } from './secrets'
import { useWorkspacesStore } from './workspaces'

const ENTITY_STORES = [useSessionStore, useStreamStore, useCatalogStore, useWorkspacesStore, useSecretsStore] as const

export function clearEntityState(pinia?: Pinia): void {
  useStreamStore(pinia).disconnect()
  for (const use of ENTITY_STORES) use(pinia).$reset()
}
