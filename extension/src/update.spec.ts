// The update path's decisions (update.ts; vscode-extension §9.1, §9.2): when
// to offer the matching release, that installing it is a mutation of the
// laptop (rule 1), and that only a release version is ever installed.

import { describe, expect, it } from 'vitest'
import { OWN_VERSION, RELOAD, installLabel, installMatching, versionNotice, type Updater } from './update'
import { FakeWindow, MemoryLog, ScriptedPrompter } from '../test/fakes'
import { ELSEWHERE } from './windowRule'
import { clone } from './flows'
import { flowWorld } from '../test/fakeDrydock'

const meta = (version: string) => ({ version, api: 1, credential: { kind: 'token', label: 'x', scopes: ['read'], expires_at: '2099-01-01T00:00:00Z' } })

class RecordingUpdater implements Updater {
  installed: string[] = []
  reloads = 0
  async install(v: string) {
    this.installed.push(v)
  }
  async reload() {
    this.reloads++
  }
}

function deps(remote?: string, answer?: (t: string, a: readonly string[]) => string | undefined) {
  const prompter = new ScriptedPrompter()
  prompter.message = async function (level, text, actions = []) {
    this.shown.push({ level, text, actions })
    return answer?.(text, actions)
  }
  return { prompter, window: new FakeWindow(remote), log: new MemoryLog(), updater: new RecordingUpdater() }
}

describe('versionNotice', () => {
  it('only when both are releases and they differ', () => {
    expect(versionNotice(meta('v9.9.9'), 'v1.0.0')).toBe('This Drydock is v9.9.9, and this extension is v1.0.0.')
    expect(versionNotice(meta('v1.0.0'), 'v1.0.0')).toBeNull()
    expect(versionNotice(meta('dev'), 'v1.0.0')).toBeNull()
    expect(versionNotice(meta('v1.0.0'), 'v0.0.0-dev')).toBeNull()
    expect(versionNotice(null)).toBeNull()
    expect(OWN_VERSION).toMatch(/^v\d+\.\d+\.\d+$/)
  })
})

describe('installMatching', () => {
  it('installs the server\'s release when the operator asks, and offers a reload', async () => {
    const d = deps(undefined, (_t, a) => a[0])
    expect(await installMatching(d, 'v9.9.9', 'Newer.')).toBe(true)
    expect(d.prompter.shown[0]!.actions).toEqual([installLabel('v9.9.9')])
    expect(d.updater.installed).toEqual(['v9.9.9'])
    expect(d.prompter.shown.at(-1)!.actions).toEqual([RELOAD])
    expect(d.updater.reloads).toBe(1)
  })

  it('declined, it installs nothing (control for the above)', async () => {
    const d = deps(undefined, () => undefined)
    expect(await installMatching(d, 'v9.9.9', 'Newer.')).toBe(false)
    expect(d.updater.installed).toEqual([])
  })

  it('from a container window it installs nothing and says why (§8 rule 1)', async () => {
    const d = deps('attached-container', (_t, a) => a[0])
    expect(await installMatching(d, 'v9.9.9', 'Newer.')).toBe(false)
    expect(d.updater.installed).toEqual([])
    expect(d.prompter.shown.map((s) => s.text)).toContain(ELSEWHERE)
  })

  it('a version that is not a release offers no install at all', async () => {
    for (const v of ['dev', 'v1.2', 'v1.2.3/../../x', 'latest']) {
      const d = deps(undefined, (_t, a) => a[0])
      expect(await installMatching(d, v, 'Newer.')).toBe(false)
      expect(d.prompter.shown[0]!.actions).toEqual([])
      expect(d.updater.installed).toEqual([])
    }
  })

  it('a server newer than the api range offers the in-editor install in place of the mutation', async () => {
    const w = flowWorld()
    w.server.api = 2
    const updater = new RecordingUpdater()
    w.updater = updater
    w.prompter.message = async function (level, text, actions = []) {
      this.shown.push({ level, text, actions })
      return actions[0]
    }
    await clone(w, 2)
    expect(w.server.mutations()).toEqual([])
    expect(updater.installed).toEqual(['v0.12.2'])
    expect(w.opener.opened).toEqual([])
  })
})
