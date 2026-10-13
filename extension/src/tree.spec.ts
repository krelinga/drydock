// The tree views' model (tree.ts; vscode-extension §6.3): the same entities
// as the hub, as trees; a node's command passes an id only to a read or a
// connect (§8 rule 2), and repository-written text is escaped.

import { describe, expect, it } from 'vitest'
import { stateEvent, stepEvent, wsView } from '../../web/src/core/reducer.fixtures'
import type { WorkspaceView } from '../../web/src/core/types'
import { repositoryNodes, workspaceNodes } from './tree'
import { COMMANDS } from './commands'
import { LINK, NEW_WS, WS, flowWorld } from '../test/fakeDrydock'
import { flush } from '../test/fakes'

describe('the tree views', () => {
  it('a running workspace connects on Enter, another opens in Drydock; each command is one that takes an id', async () => {
    const w = flowWorld()
    w.server.workspaces.push(wsView({ id: '01JA0000000000000000000011', repository_id: 2, full_name: 'krelinga/homelab', state: 'stopped' }))
    const release = await w.live.hold()
    const nodes = workspaceNodes(w.live.entities)
    release()
    expect(nodes.map((n) => [n.label, n.contextValue, n.command])).toEqual([
      ['krelinga/drydock', 'workspace.running', { id: 'drydock.connect', arg: WS }],
      ['krelinga/homelab', 'workspace.stopped', { id: 'drydock.openInBrowser', arg: '01JA0000000000000000000011' }],
    ])
    for (const n of nodes) {
      const def = COMMANDS.find((c) => c.id === n.command!.id)!
      expect(def.mutating).toBe(false)
      expect('takesId' in def && def.takesId).toBe(true)
    }
    expect(nodes[0]!.tooltip).toContain('Connects over SSH to devbox')
  })

  it("a workspace's children are its steps, in order, as the timeline", async () => {
    const w = flowWorld()
    const release = await w.live.hold()
    w.server.emit(stateEvent(300, NEW_WS, 'pending', { repository_id: 2 }), stepEvent(301, NEW_WS, 'allocate', 'done'), stepEvent(302, NEW_WS, 'clone', 'started'))
    await flush()
    const n = workspaceNodes(w.live.entities).find((x) => x.key === NEW_WS)!
    release()
    expect(n.children!.map((c) => [c.label, c.description, c.icon])).toEqual([
      ['Allocating', 'done', 'pass'],
      ['Cloning', 'started', 'loading~spin'],
    ])
  })

  it('repositories without a workspace, clonable, or saying the cap; no node passes anything to clone', async () => {
    const w = flowWorld()
    const release = await w.live.hold()
    expect(repositoryNodes(w.live.entities).map((n) => [n.label, n.contextValue, n.command])).toEqual([['krelinga/homelab', 'repo.clonable', undefined]])
    w.server.cap = 1
    await w.live.refetch()
    expect(repositoryNodes(w.live.entities)[0]).toMatchObject({ contextValue: 'repo.atcap', description: 'At the cap (1): stop a workspace first' })
    release()
  })

  it('repository-written names are escaped', async () => {
    const w = flowWorld()
    w.server.workspaces[0] = { ...w.server.workspaces[0]!, full_name: 'evil/$(alert)\u202Ex', vscode: { configured: true, url: LINK } } as WorkspaceView
    const release = await w.live.hold()
    expect(workspaceNodes(w.live.entities)[0]!.label).toBe('evil/\\$(alert)\\u202Ex')
    release()
  })
})
