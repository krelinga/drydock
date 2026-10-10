// The Open in VS Code link through the reducer (design §6, "Opening a
// workspace in VS Code"): written by the views alone, validated before it is
// stored, and shown only while running and no older than the state it opens.

import { describe, expect, it } from 'vitest'
import type { WorkspaceView } from '../api/types'
import { newBackend, vscodeURLFor, WS_RUNNING, workspaceList } from '../mocks/backend'
import { WS, listBody, stateEvent, wsView } from './reducer.fixtures'
import {
  becameRunning, emptyEntities, reduce, reduceAll, vscodeConfigured, vscodeLink, vscodeURL, type Entities,
} from './reducer'

// internal/vscode's golden shape: {"containerName":"/mock_1"} in hex, then
// Remote-SSH's authority for owner@devbox.lan, {"hostName","user"} in hex.
const GOOD =
  'vscode://vscode-remote/attached-container+7b22636f6e7461696e65724e616d65223a222f6d6f636b5f31227d' +
  '@ssh-remote+7b22686f73744e616d65223a22646576626f782e6c616e222c2275736572223a226f776e6572227d/workspaces/repo1'

const list = (at: number, over: Partial<WorkspaceView>): Entities =>
  reduce(emptyEntities(), { type: 'workspaces', at, view: listBody(wsView(over)) })

describe('the VS Code link', () => {
  it('comes from the view, for a running workspace', () => {
    const e = list(20, { vscode: { configured: true, url: GOOD } })
    expect(vscodeLink(e.workspaces[WS]!)).toBe(GOOD)
    expect(vscodeConfigured(e.workspaces[WS]!)).toBe(true)
  })

  it('a body without the field says nothing, and keeps what was known', () => {
    const none = list(20, {})
    expect(vscodeConfigured(none.workspaces[WS]!)).toBeNull()
    expect(vscodeLink(none.workspaces[WS]!)).toBeNull()
    const known = list(20, { vscode: { configured: true, url: GOOD } })
    const later = reduce(known, { type: 'workspaces', at: 30, view: listBody(wsView()) })
    expect(later.workspaces[WS]!.vscode).toEqual({ configured: true, url: GOOD })
    expect(vscodeConfigured(later.workspaces[WS]!)).toBe(true)
    // …but the link itself is older than the state that body wrote.
    expect(vscodeLink(later.workspaces[WS]!)).toBeNull()
  })

  it('unconfigured is said, with no link', () => {
    const e = list(20, { vscode: { configured: false, url: GOOD } })
    expect(vscodeConfigured(e.workspaces[WS]!)).toBe(false)
    expect(vscodeLink(e.workspaces[WS]!)).toBeNull()
  })

  it('never stores a URL internal/vscode could not have built', () => {
    for (const bad of [
      'javascript:alert(1)',
      'https://evil.example/',
      'vscode://vscode-remote/ssh-remote+devbox/etc',
      'vscode://vscode-remote/attached-container+7b22@ssh-remote+devbox/a"onmouseover=x',
      'vscode://vscode-remote/attached-container+7B22@ssh-remote+devbox/w',
      'vscode://vscode-remote/attached-container+7b22@ssh-remote+devbox',
      'vscode://vscode-remote/attached-container+7b22@ssh-remote+dev box/w',
      'vscode://other-extension/attached-container+7b22@ssh-remote+devbox/w',
      42,
    ]) {
      expect(vscodeURL(bad), String(bad)).toBeNull()
      const e = list(20, { vscode: { configured: true, url: bad as string } })
      expect(vscodeLink(e.workspaces[WS]!), String(bad)).toBeNull()
    }
    expect(vscodeURL(GOOD)).toBe(GOOD) // control
  })

  it('is hidden once a state event is newer than the view that carried it', () => {
    const e = list(20, { vscode: { configured: true, url: GOOD } })
    expect(vscodeLink(e.workspaces[WS]!)).toBe(GOOD) // control
    // A rebuild: building, then running in a new container. The link names
    // the old one until a view taken after the move says otherwise.
    const rebuilt = reduceAll(e, [
      { type: 'event', event: stateEvent(21, WS, 'building', { from: 'running' }) },
      { type: 'event', event: stateEvent(22, WS, 'running', { from: 'building', container_id: 'c0ffee99' }) },
    ])
    expect(rebuilt.workspaces[WS]!.state).toBe('running')
    expect(vscodeLink(rebuilt.workspaces[WS]!)).toBeNull()
    const refetched = reduce(rebuilt, { type: 'workspaces', at: 22, view: listBody(wsView({ vscode: { configured: true, url: GOOD } })) })
    expect(vscodeLink(refetched.workspaces[WS]!)).toBe(GOOD)
  })

  it('is none for a workspace that is not running', () => {
    const e = list(20, { state: 'stopped', vscode: { configured: true, url: GOOD } })
    expect(vscodeLink(e.workspaces[WS]!)).toBeNull()
  })

  it('a move to running is what makes a view refetch', () => {
    expect(becameRunning(stateEvent(5, WS, 'running'))).toBe(true)
    expect(becameRunning(stateEvent(5, WS, 'running'), WS)).toBe(true)
    expect(becameRunning(stateEvent(5, WS, 'running'), 'other')).toBe(false)
    expect(becameRunning(stateEvent(5, WS, 'building'))).toBe(false)
  })
})

describe('the mock and the reducer agree', () => {
  it("the mock's list, through the reducer, links its running workspace and no other", () => {
    const b = newBackend({ vscodeHost: 'owner@devbox.lan' })
    const e = reduce(emptyEntities(), { type: 'workspaces', at: 1, view: workspaceList(b) })
    expect(vscodeLink(e.workspaces[WS_RUNNING]!)).toBe(GOOD)
    for (const w of Object.values(e.workspaces)) {
      if (w.id !== WS_RUNNING) {
        expect(vscodeLink(w)).toBeNull()
        expect(vscodeConfigured(w)).toBe(true)
      }
    }
  })

  it("a bare lowercase host is Remote-SSH's plain authority", () => {
    expect(vscodeURLFor('devbox', '/x', '/workspaces/r')).toBe(
      'vscode://vscode-remote/attached-container+7b22636f6e7461696e65724e616d65223a222f78227d@ssh-remote+devbox/workspaces/r')
    expect(vscodeURL(vscodeURLFor('devbox', '/x', '/workspaces/r'))).not.toBeNull()
  })

  it('unconfigured and silent mocks match the server', () => {
    const off = reduce(emptyEntities(), { type: 'workspaces', at: 1, view: workspaceList(newBackend({ vscodeHost: null })) })
    expect(vscodeConfigured(off.workspaces[WS_RUNNING]!)).toBe(false)
    const silent = reduce(emptyEntities(), { type: 'workspaces', at: 1, view: workspaceList(newBackend()) })
    expect(vscodeConfigured(silent.workspaces[WS_RUNNING]!)).toBeNull()
  })
})
