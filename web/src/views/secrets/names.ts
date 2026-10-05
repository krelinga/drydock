import type { Entities } from '../../stores/reducer'

/**
 * A workspace named by id in `accessed_by`, as a person reads it: its
 * repository when the entities know it, otherwise the id's tail. A workspace
 * can have been deleted since it fetched, and "which workspaces ever held
 * this?" (design §10.4) must still answer with something.
 */
export function workspaceName(e: Entities, id: string): string {
  const w = e.workspaces[id]
  if (w !== undefined) {
    const repo = w.repositoryId === null ? undefined : e.repos[w.repositoryId]
    const name = w.fullName ?? repo?.fullName
    if (name) return name
  }
  return `workspace …${id.slice(-6)}`
}
