// The two tree views (vscode-extension §6.3), the secondary surface: what the
// hub shows, as trees. *Workspaces* lists every workspace, its status line
// from core's card logic, and its eight steps as children (§10.5: the web
// UI's timeline). *Repositories* lists the repositories with no workspace.
//
// A node's command passes a workspace id only to a command that reads or
// connects (§8 rule 2): Enter on a running workspace connects, on any other
// opens its page in Drydock. A repository has no default command; its
// context menu's *Clone…* runs `drydock.clone`, which asks for itself which
// repository. `contextValue` drives the menus (package.json).

import { WORKSPACE_STEPS } from '../../web/src/core/types'
import { capacity } from '../../web/src/core/lib/capacity'
import { cardStatus, stepTitle } from '../../web/src/core/lib/workspaceCard'
import { repoHeld, runSteps, vscodeSSHHost, workspaceForRepo, type Entities, type Workspace } from '../../web/src/core/reducer'
import { atCap, pickText } from './hub'

export interface TreeNode {
  /** Stable across renders. */
  key: string
  label: string
  description?: string
  tooltip?: string
  /** A codicon name. */
  icon: string
  contextValue: string
  command?: { id: 'drydock.connect' | 'drydock.openInBrowser' | 'drydock.reviewApproval'; arg: string }
  children?: TreeNode[]
}

const nameOf = (w: Workspace, e: Entities) =>
  pickText(w.fullName ?? (w.repositoryId !== null ? e.repos[w.repositoryId]?.fullName : undefined) ?? w.id)

const STEP_ICON: Record<string, string> = { started: 'loading~spin', done: 'pass', failed: 'error', needs_approval: 'shield' }

export function workspaceNodes(e: Entities): TreeNode[] {
  return Object.values(e.workspaces)
    .filter((w) => w.state !== null && e.gone[w.id] === undefined)
    .sort((a, b) => Number(b.state === 'running') - Number(a.state === 'running') || nameOf(a, e).localeCompare(nameOf(b, e)))
    .map((w) => {
      const s = cardStatus(w)
      const running = w.state === 'running'
      const host = vscodeSSHHost(w.vscode?.url ?? null)
      const steps = runSteps(w)
      const children: TreeNode[] = WORKSPACE_STEPS.filter((name) => steps[name] !== undefined).map((name) => ({
        key: `${w.id}/${name}`,
        label: stepTitle(name),
        description: steps[name]!.status.replace('_', ' '),
        tooltip: steps[name]!.detail ?? undefined,
        icon: STEP_ICON[steps[name]!.status] ?? 'circle-outline',
        contextValue: 'step',
      }))
      return {
        key: w.id,
        label: nameOf(w, e),
        description: w.approval !== null ? 'Needs host-access approval' : s.line,
        tooltip: [s.note, w.branch !== null ? `Branch ${pickText(w.branch)}` : null, running && host !== null ? `Connects over SSH to ${pickText(host)}` : null]
          .filter((x) => x !== null).join('\n') || undefined,
        icon: running ? 'vm-running' : w.state === 'failed' ? 'vm-outline' : 'vm',
        contextValue: w.approval !== null && w.state === 'stopped' ? 'workspace.needsApproval' : `workspace.${w.state}`,
        // Opening the review document reads; it never approves (§10.4).
        command: running ? { id: 'drydock.connect', arg: w.id }
          : w.approval !== null && w.state === 'stopped' ? { id: 'drydock.reviewApproval', arg: w.id }
          : { id: 'drydock.openInBrowser', arg: w.id },
        children,
      }
    })
}

export function repositoryNodes(e: Entities): TreeNode[] {
  const cap = capacity(e)
  const out: TreeNode[] = []
  for (const id of e.repoOrder) {
    const repo = e.repos[id]
    if (repo === undefined || repo.removed || workspaceForRepo(e, id) !== null || repoHeld(e, id)) continue
    out.push({
      key: `repo/${id}`,
      label: pickText(repo.fullName),
      description: cap.full && cap.cap !== null ? atCap(cap.cap) : repo.archived ? 'archived' : undefined,
      icon: 'repo',
      contextValue: cap.full ? 'repo.atcap' : 'repo.clonable',
    })
  }
  return out
}
