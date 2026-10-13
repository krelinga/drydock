// E1's flows over a Drydock in memory (test/fakeDrydock.ts; vscode-extension
// §12.2): the hub's order and actions, clone and follow, connect and every
// way it cannot, the refresh, and the skew gate. Negative assertions read the
// fake's request log; each has its control beside it.

import { describe, expect, it } from 'vitest'
import { stateEvent, stepEvent, wsView } from '../../web/src/core/reducer.fixtures'
import type { WorkspaceView } from '../../web/src/core/types'
import {
  CONNECT, DEV_CONTAINERS, INSTALL_REMOTES, NEEDS_REMOTES, NO_CONTAINER, NO_SSH_HOST, OPEN_IN_DRYDOCK, REMOTE_SSH, TRY_AGAIN,
  STREAM_ENDED, clone, cloneCommand, connect, followState, go, refreshRepositories,
} from './flows'
import { atCap, hubItems } from './hub'
import { IDLE_MS } from './live'
import { SERVER_NEWER } from './handshake'
import { ELSEWHERE } from './windowRule'
import { LINK, NEW_WS, WS, event, flowWorld, type FlowWorld } from '../test/fakeDrydock'
import { FakeStream, ORIGIN, flush } from '../test/fakes'

const REMOTE = LINK.replace('vscode://vscode-remote/', 'vscode-remote://')

/** Answers each message with the action `pick` chooses (by default, none). */
function answering(w: FlowWorld, pick: (text: string, actions: readonly string[]) => string | undefined = () => undefined) {
  w.prompter.message = async function (level, text, actions = []) {
    this.shown.push({ level, text, actions })
    return pick(text, actions)
  }
}

/** The stream open and the first snapshots in, as any command's hold leaves it. */
async function opened(w: FlowWorld): Promise<() => void> {
  const p = w.live.hold()
  await flush()
  return p
}

describe('the hub', () => {
  it('running workspaces first (connect), then the others (open in Drydock), then repositories without one (clone)', async () => {
    const w = flowWorld()
    w.server.workspaces.push(wsView({ id: '01JA0000000000000000000011', repository_id: 3, full_name: 'krelinga/zeta', state: 'failed' }))
    w.server.catalog.repos.push({ ...w.server.catalog.repos[1]!, id: 3, full_name: 'krelinga/zeta', workspace: { id: '01JA0000000000000000000011', state: 'failed' } })
    const release = await opened(w)
    const items = hubItems(w.live.entities)
    release()
    expect(items.map((i) => [i.label, i.value.kind])).toEqual([
      ['$(vm-running) krelinga/drydock', 'connect'],
      ['$(vm) krelinga/zeta', 'open'],
      ['$(repo) krelinga/homelab', 'clone'],
    ])
  })

  it('a workspace this window followed to running is first, marked ready', async () => {
    const w = flowWorld()
    w.server.workspaces.push(wsView({ id: '01JA0000000000000000000011', repository_id: 2, full_name: 'krelinga/alpha', state: 'running' }))
    const release = await opened(w)
    expect(hubItems(w.live.entities).map((i) => i.label)[0]).toBe('$(vm-running) krelinga/alpha')
    const ready = hubItems(w.live.entities, new Set([WS]))
    release()
    expect(ready[0]).toMatchObject({ label: '$(vm-running) krelinga/drydock' })
    expect(ready[0]!.description).toMatch(/^ready · /)
  })

  it('at the cap a repository says so and Enter opens Drydock; never a disabled item, and nothing is sent', async () => {
    const w = flowWorld()
    w.server.cap = 1
    answering(w, (_t, a) => a[0])
    const release = await opened(w)
    const item = hubItems(w.live.entities).find((i) => i.label === '$(repo) krelinga/homelab')!
    release()
    expect(item).toMatchObject({ description: atCap(1), value: { kind: 'make_room' } })
    w.prompter.pickAnswers = [1]
    await go(w)
    expect(w.server.mutations()).toEqual([])
    expect(w.opener.opened).toEqual([`${ORIGIN}/`])
  })

  it('Enter on a running workspace connects; on another opens its page in Drydock', async () => {
    const w = flowWorld()
    w.prompter.pickAnswers = [0]
    await go(w)
    expect(w.opener.folders).toEqual([REMOTE])
    w.server.workspaces.push(wsView({ id: '01JA0000000000000000000011', repository_id: 3, full_name: 'krelinga/zeta', state: 'stopped' }))
    await w.live.refetch()
    w.prompter.pickAnswers = [1]
    await go(w)
    expect(w.opener.opened).toEqual([`${ORIGIN}/ws/01JA0000000000000000000011`])
  })
})

describe('clone and follow', () => {
  function cloning(w: FlowWorld) {
    // After the create's own event, the steps and the move to running.
    w.server.onCreate = (repoId) => [
      event(200, 'workspace.state', NEW_WS, { state: 'pending', repository_id: repoId, branch: 'main' }),
      event(201, 'workspace.state', NEW_WS, { state: 'cloning', from: 'pending' }),
      { ...stepEvent(202, NEW_WS, 'clone', 'started') },
    ]
  }

  it('sends the create, learns the workspace from the stream (never the 202), follows it to running and offers Connect', async () => {
    const w = flowWorld()
    cloning(w)
    answering(w, (t) => (t.includes(' is running.') ? CONNECT : undefined))
    w.server.workspaces.push({ ...wsView({ id: NEW_WS, repository_id: 2, full_name: 'krelinga/homelab', state: 'running' }), vscode: { configured: true, url: LINK } } as WorkspaceView)
    const done = clone(w, 2)
    await flush()
    expect(w.server.sent('POST', '/api/workspaces').map((r) => JSON.parse(r.body!))).toEqual([{ repository_id: 2 }])
    expect(w.prompter.progresses[0]?.title).toBe('Drydock: krelinga/homelab')
    w.server.emit(stateEvent(203, NEW_WS, 'running', { from: 'building', container_id: 'c1' }))
    await done
    expect(w.prompter.progresses[0]!.reports).toContain('Cloning…')
    expect(w.prompter.shown.map((s) => s.text)).toContain('krelinga/homelab is running. Connect opens it over SSH to devbox.')
    // Connect used the workspace the stream named, refetched for its link.
    expect(w.server.sent('GET', `/api/workspaces/${NEW_WS}`).length).toBeGreaterThan(0)
    expect(w.server.sent('GET', '/api/workspaces/01JA00000000000000000DECOY')).toEqual([])
    expect(w.opener.folders).toEqual([REMOTE])
    expect(w.ready.has(NEW_WS)).toBe(true)
  })

  it('drydock.connectWhenReady connects without asking', async () => {
    const w = flowWorld()
    cloning(w)
    w.connectNow.value = true
    answering(w)
    w.server.workspaces.push({ ...wsView({ id: NEW_WS, repository_id: 2, state: 'running' }), vscode: { configured: true, url: LINK } } as WorkspaceView)
    const done = clone(w, 2)
    await flush()
    w.server.emit(stateEvent(203, NEW_WS, 'running', { from: 'building' }))
    await done
    expect(w.prompter.shown).toEqual([])
    expect(w.opener.folders).toEqual([REMOTE])
  })

  it('a failure, a host-access request and a dismissal each end the follow as themselves', () => {
    const w = flowWorld()
    w.live.dispatch({ type: 'event', event: event(1, 'workspace.state', NEW_WS, { state: 'pending', repository_id: 2 }) })
    expect(followState(w.live.entities, NEW_WS).end).toBeNull()
    w.live.dispatch({ type: 'event', event: event(2, 'workspace.state', NEW_WS, { state: 'failed', from: 'building', detail: 'up failed' }) })
    expect(followState(w.live.entities, NEW_WS).end).toBe('failed')
    w.live.dispatch({ type: 'event', event: event(3, 'workspace.state', NEW_WS, {
      state: 'stopped', from: 'failed', approval: { hash: 'sha256:a', added: [{ field: 'privileged', source: 'feature_or_image', value: true }], changed: [], removed: [] },
    }) })
    expect(followState(w.live.entities, NEW_WS).end).toBe('approval')
    // The request a follow's own approval sent is not its end; another one is.
    expect(followState(w.live.entities, NEW_WS, 'sha256:a').end).toBeNull()
    expect(followState(w.live.entities, NEW_WS, 'sha256:other').end).toBe('approval')
    w.live.dispatch({ type: 'event', event: event(4, 'workspace.gone', NEW_WS, {}) })
    expect(followState(w.live.entities, NEW_WS).end).toBe('gone')
  })

  it('a follow holds the stream past five idle minutes, and releases it when it ends (§5.5)', async () => {
    const w = flowWorld()
    cloning(w)
    answering(w)
    const done = clone(w, 2)
    await flush()
    // Six minutes, the server's heartbeat arriving every 20 s as it does.
    for (let t = 0; t < IDLE_MS + 60_000; t += 20_000) {
      w.clock.advance(20_000)
      w.server.stream!.push(': ping\n\n')
      await flush()
    }
    expect(w.live.streaming).toBe(true)
    expect(w.server.streams).toBe(1)
    w.server.emit(stateEvent(203, NEW_WS, 'failed', { from: 'building' }))
    await done
    expect(w.prompter.shown.at(-1)).toMatchObject({ level: 'error', actions: [OPEN_IN_DRYDOCK] })
    w.clock.advance(IDLE_MS)
    await flush()
    expect(w.live.streaming).toBe(false)
  })

  it('from a container window, the hub\'s clone sends nothing and says to do it elsewhere; from a local one it sends (control)', async () => {
    const w = flowWorld('attached-container')
    answering(w)
    await clone(w, 2)
    expect(w.server.mutations()).toEqual([])
    expect(w.prompter.shown.map((s) => s.text)).toEqual([ELSEWHERE])
    const local = flowWorld()
    answering(local)
    local.prompter.dismissNow = true
    await clone(local, 2)
    expect(local.server.sent('POST', '/api/workspaces')).toHaveLength(1)
  })

  it('at the cap, Clone Repository… says so and sends nothing', async () => {
    const w = flowWorld()
    w.server.cap = 1
    answering(w)
    await cloneCommand(w)
    expect(w.server.mutations()).toEqual([])
    expect(w.prompter.shown[0]!.text).toContain(atCap(1))
  })

  it('a server newer than the extension\'s api range refuses the mutation, offering the matching release; a reader still works', async () => {
    const w = flowWorld()
    w.server.api = 2
    answering(w, (_t, a) => a[0])
    await clone(w, 2)
    expect(w.server.mutations()).toEqual([])
    expect(w.prompter.shown[0]!.text).toContain(SERVER_NEWER)
    expect(w.opener.opened).toEqual(['https://github.com/krelinga/drydock/releases/download/v0.12.2/drydock-vscode.vsix'])
    w.prompter.pickAnswers = [0]
    await go(w)
    expect(w.opener.folders).toEqual([REMOTE])
  })
})

describe('a follow ends when the stream does', () => {
  it('signed out under it: the notification closes and says why', async () => {
    const w = flowWorld()
    answering(w)
    const done = clone(w, 2)
    await flush()
    expect(w.prompter.progresses).toHaveLength(1)
    w.live.close('signed_out')
    w.clock.advance(1_000)
    await done
    expect(w.prompter.shown.at(-1)!.text).toBe(`Stopped following krelinga/homelab: ${STREAM_ENDED.signed_out}`)
  })

  // The host tier's race (a sign-in's late credential change): the clone's
  // hold was taken on the stream about to close, so the close lands after
  // the create is sent and before its event arrives. Nothing can settle the
  // mark then; the clone must end and say why, not wait forever.
  it('closed between the hold and the create\'s event: the clone ends, says why and leaves no mark; the stream open is the control', async () => {
    const w = flowWorld()
    answering(w)
    const release = await opened(w)
    const done = clone(w, 2)
    let ended = false
    void done.then(() => { ended = true }, () => { ended = true })
    w.live.close('signed_in_again')
    for (let i = 0; i < 5; i++) {
      await flush()
      w.clock.advance(1_000)
    }
    await flush()
    expect(w.server.sent('POST', '/api/workspaces')).toHaveLength(1)
    expect(ended).toBe(true)
    expect(w.prompter.shown.at(-1)!.text).toBe(`Stopped following krelinga/homelab: ${STREAM_ENDED.signed_in_again}`)
    expect(w.live.marks.inFlight).toEqual({})
    expect(w.live.held).toBe(true)
    release()
    expect(w.live.held).toBe(false)

    // Control: the same clone with the stream left open learns its workspace and follows it.
    const c = flowWorld()
    answering(c)
    const r = await opened(c)
    void clone(c, 2)
    await flush()
    expect(c.prompter.progresses).toHaveLength(1)
    expect(c.live.marks.inFlight).toEqual({})
    r()
  })

  it('a 401 on the reconnect: the same, with the token sentence; a follow that reaches running is the control', async () => {
    const w = flowWorld()
    answering(w)
    const done = clone(w, 2)
    await flush()
    w.server.transport.streamRoute = () => new FakeStream(401)
    w.server.stream!.end()
    await flush()
    w.clock.advance(1_000)
    await flush()
    w.clock.advance(1_000)
    await done
    expect(w.live.endedBy).toBe('unauthorized')
    expect(w.prompter.shown.at(-1)!.text).toBe(`Stopped following krelinga/homelab: ${STREAM_ENDED.unauthorized}`)
  })
})

describe('what the operator sees of repository-written text and the SSH host', () => {
  it('a name carrying $(…) or a bidi override cannot draw an icon or reorder the pick', async () => {
    const w = flowWorld()
    w.server.catalog.repos[1] = { ...w.server.catalog.repos[1]!, full_name: 'evil/$(warning)x\u202E' }
    const release = await opened(w)
    const labels = hubItems(w.live.entities).map((i) => i.label)
    release()
    expect(labels).toContain('$(repo) evil/\\$(warning)x\\u202E')
    // Control: the extension's own icons are untouched.
    expect(labels[0]).toBe('$(vm-running) krelinga/drydock')
  })

  it('the SSH host the server chose is shown on the hub item and when a follow ends at running', async () => {
    const w = flowWorld()
    const release = await opened(w)
    expect(hubItems(w.live.entities)[0]!.detail).toBe('main · over SSH to devbox')
    release()
    answering(w)
    w.server.onCreate = (repoId) => [event(200, 'workspace.state', NEW_WS, { state: 'pending', repository_id: repoId })]
    w.server.workspaces.push({ ...wsView({ id: NEW_WS, repository_id: 2, state: 'running' }), vscode: { configured: true, url: LINK } } as WorkspaceView)
    const done = clone(w, 2)
    await flush()
    w.server.emit(stateEvent(203, NEW_WS, 'running', { from: 'building' }))
    await flush()
    await done
    expect(w.prompter.shown.map((x) => x.text)).toContain('krelinga/homelab is running. Connect opens it over SSH to devbox.')
  })
})

describe('connect', () => {
  it('opens the vscode-remote form of the link from a view fetched now', async () => {
    const w = flowWorld()
    await connect(w, WS)
    expect(w.server.sent('GET', `/api/workspaces/${WS}`).length).toBeGreaterThan(0)
    expect(w.opener.folders).toEqual([REMOTE])
    expect(w.opener.opened).toEqual([])
  })

  it('no --vscode-ssh-host: says so, offers Drydock, opens nothing', async () => {
    const w = flowWorld()
    w.server.workspaces[0] = { ...w.server.workspaces[0]!, vscode: { configured: false, url: null } } as WorkspaceView
    answering(w)
    await connect(w, WS)
    expect(w.prompter.shown[0]).toMatchObject({ text: NO_SSH_HOST, actions: [OPEN_IN_DRYDOCK] })
    expect(w.opener.folders).toEqual([])
  })

  it('no link (the container not found): says so with Try again, which looks again', async () => {
    const w = flowWorld()
    w.server.workspaces[0] = { ...w.server.workspaces[0]!, vscode: { configured: true, url: null } } as WorkspaceView
    let tries = 0
    answering(w, (t) => (t === NO_CONTAINER && tries++ === 0 ? TRY_AGAIN : undefined))
    await connect(w, WS)
    expect(w.prompter.shown.map((s) => s.text)).toEqual([NO_CONTAINER, NO_CONTAINER])
    expect(w.server.sent('GET', `/api/workspaces/${WS}`)).toHaveLength(2)
    expect(w.opener.folders).toEqual([])
  })

  it('a link of the wrong shape is no link', async () => {
    const w = flowWorld()
    w.server.workspaces[0] = { ...w.server.workspaces[0]!, vscode: { configured: true, url: 'vscode://evil.test/x' } } as WorkspaceView
    answering(w)
    await connect(w, WS)
    expect(w.prompter.shown[0]!.text).toBe(NO_CONTAINER)
    expect(w.opener.folders).toEqual([])
  })

  it('not running: says so, opens nothing', async () => {
    const w = flowWorld()
    w.server.workspaces[0] = { ...w.server.workspaces[0]!, state: 'stopped' }
    answering(w)
    await connect(w, WS)
    expect(w.prompter.shown[0]!.text).toContain('is not running')
    expect(w.opener.folders).toEqual([])
  })

  it('without Remote - SSH and Dev Containers: offers to install both, then connects; declined, opens nothing', async () => {
    const w = flowWorld()
    w.installed.clear()
    answering(w, (t) => (t === NEEDS_REMOTES ? INSTALL_REMOTES : undefined))
    await connect(w, WS)
    expect([...w.installed].sort()).toEqual([DEV_CONTAINERS, REMOTE_SSH].sort())
    expect(w.opener.folders).toEqual([REMOTE])
    const no = flowWorld()
    no.installed.clear()
    answering(no)
    await connect(no, WS)
    expect(no.installed.size).toBe(0)
    expect(no.opener.folders).toEqual([])
  })

  it('in a container window, connect still works but installs nothing (§8 rule 1)', async () => {
    const w = flowWorld('attached-container')
    answering(w, () => INSTALL_REMOTES)
    await connect(w, WS)
    expect(w.opener.folders).toEqual([REMOTE])
    w.installed.clear()
    await connect(w, WS)
    expect(w.installed.size).toBe(0)
    expect(w.prompter.shown.at(-1)!.text).toBe(`${NEEDS_REMOTES} ${ELSEWHERE}`)
  })
})

describe('refresh', () => {
  it('sends the refresh, ends on its outcome event, and says what it said', async () => {
    const w = flowWorld()
    answering(w)
    await refreshRepositories(w)
    expect(w.server.sent('POST', '/api/repos/refresh')).toHaveLength(1)
    expect(w.live.marks.has('catalog:refresh')).toBe(false)
    expect(w.prompter.shown.at(-1)).toMatchObject({ level: 'info' })
  })
})
