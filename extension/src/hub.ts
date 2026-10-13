// The hub, *Go to Workspace…* (vscode-extension §6.1, §6.2): one quick pick,
// computed from the entities by core's own card logic, so it orders and
// words things as the web UI's home does.
//
//   1. Running workspaces — Enter connects. One this window followed to
//      running is marked ready and comes first, so the shortcut and Enter
//      connect to it.
//   2. Other workspaces — Enter opens them in Drydock (E1; E2 adds their
//      actions here). One waiting on a host-access request says so.
//   3. Repositories without a workspace — Enter clones. At the cap the item
//      says so, and Enter opens Drydock to make room: never a disabled item.
//
// Repository-written text (a full name, a branch) is shown escaped (core's
// escapeFormat), as the web UI shows it.

import { capacity } from '../../web/src/core/lib/capacity'
import { escapeFormat } from '../../web/src/core/lib/text'
import { cardStatus, rowAction, type FleetLogin } from '../../web/src/core/lib/workspaceCard'
import { repoHeld, workspaceForRepo, type Entities, type Workspace } from '../../web/src/core/reducer'
import type { PickItem } from './ports'

export type HubAction =
  | { kind: 'connect'; id: string }
  | { kind: 'open'; id: string }
  | { kind: 'clone'; repoId: number }
  | { kind: 'make_room' }

const nameOf = (w: Workspace, e: Entities) =>
  escapeFormat(w.fullName ?? (w.repositoryId !== null ? e.repos[w.repositoryId]?.fullName : undefined) ?? w.id)

/** The cap sentence a clone item carries when Drydock is full (§6.1, never a disabled item). */
export const atCap = (cap: number) => `At the cap (${cap}): stop a workspace first`

export function hubItems(e: Entities, ready: ReadonlySet<string> = new Set(), fleet: FleetLogin = null): PickItem<HubAction>[] {
  const all = Object.values(e.workspaces).filter((w) => w.state !== null && e.gone[w.id] === undefined)
  const byName = (a: Workspace, b: Workspace) => nameOf(a, e).localeCompare(nameOf(b, e))
  const running = all.filter((w) => w.state === 'running').sort((a, b) =>
    Number(ready.has(b.id)) - Number(ready.has(a.id)) || byName(a, b))
  const others = all.filter((w) => w.state !== 'running').sort(byName)
  const items: PickItem<HubAction>[] = []

  for (const w of running) {
    const s = cardStatus(w, fleet)
    items.push({
      label: `$(vm-running) ${nameOf(w, e)}`,
      description: ready.has(w.id) ? `ready · ${s.line}` : s.line,
      detail: w.branch !== null ? escapeFormat(w.branch) : undefined,
      value: { kind: 'connect', id: w.id },
    })
  }
  for (const w of others) {
    const s = cardStatus(w, fleet)
    items.push({
      label: `$(vm) ${nameOf(w, e)}`,
      description: w.approval !== null ? 'Needs host-access approval: open in Drydock' : `${s.line}: open in Drydock`,
      value: { kind: 'open', id: w.id },
    })
  }
  const cap = capacity(e)
  for (const id of e.repoOrder) {
    const repo = e.repos[id]
    if (repo === undefined) continue
    const ws = workspaceForRepo(e, id)
    if (ws !== null) continue
    if (rowAction({ repo, workspace: null, held: repoHeld(e, id) }) !== 'clone') continue
    if (cap.full && cap.cap !== null) {
      items.push({ label: `$(repo) ${escapeFormat(repo.fullName)}`, description: atCap(cap.cap), value: { kind: 'make_room' } })
    } else {
      items.push({ label: `$(repo) ${escapeFormat(repo.fullName)}`, description: 'Clone', value: { kind: 'clone', repoId: id } })
    }
  }
  return items
}

/** Running workspaces only, for *Connect to Workspace…*. */
export function connectItems(e: Entities, ready: ReadonlySet<string> = new Set()): PickItem<string>[] {
  return hubItems(e, ready).flatMap((i) => (i.value.kind === 'connect' ? [{ ...i, value: i.value.id }] : []))
}

/** Repositories a clone can start for, for *Clone Repository…*. At the cap: none, and the caller says why. */
export function cloneItems(e: Entities): PickItem<number>[] {
  return hubItems(e).flatMap((i) => (i.value.kind === 'clone' ? [{ ...i, value: i.value.repoId }] : []))
}
