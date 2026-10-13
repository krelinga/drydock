// The manifest is a tested artifact (vscode-extension §12.3): most "does the
// palette and keyboard work" questions are about package.json, and need no
// editor. Each check is a function of a manifest, run against the real one
// and against a broken fixture that must fail it by name (the controls).

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { COMMANDS } from '../src/commands'

type Manifest = Record<string, any>
const real: Manifest = JSON.parse(readFileSync(resolve(process.cwd(), 'package.json'), 'utf8'))
const typesVersion: string = JSON.parse(readFileSync(resolve(process.cwd(), 'node_modules/@types/vscode/package.json'), 'utf8')).version

/** Commands deliberately hidden from the palette, each with its reason. None yet: every E1 command is in the palette. */
const HIDDEN_FROM_PALETTE: Record<string, string> = {}

function commandErrors(m: Manifest): string[] {
  const errs: string[] = []
  const contributed: Array<{ command: string; title: string; category: string }> = m.contributes?.commands ?? []
  for (const c of contributed) {
    if (!/^drydock\.[a-zA-Z]+$/.test(c.command ?? '')) errs.push(`command ${c.command} is not drydock.*`)
    if (c.category !== 'Drydock') errs.push(`command ${c.command} has category ${c.category}`)
  }
  const ids = new Set(contributed.map((c) => c.command))
  const registered = new Set<string>(COMMANDS.map((c) => c.id))
  for (const id of registered) if (!ids.has(id)) errs.push(`command ${id} is registered but not contributed`)
  for (const id of ids) if (!registered.has(id)) errs.push(`command ${id} is contributed but never registered`)
  for (const c of contributed) {
    const def = COMMANDS.find((d) => d.id === c.command)
    if (def !== undefined && def.title !== c.title) errs.push(`command ${c.command} is titled ${c.title}, the registry says ${def.title}`)
  }
  const palette: Array<{ command: string; when?: string }> = m.contributes?.menus?.commandPalette ?? []
  for (const p of palette) {
    if (p.when === 'false' && HIDDEN_FROM_PALETTE[p.command] === undefined) errs.push(`command ${p.command} is hidden from the palette`)
  }
  return errs
}

function shapeErrors(m: Manifest): string[] {
  const errs: string[] = []
  if (JSON.stringify(m.extensionKind) !== '["ui"]') errs.push(`extensionKind is ${JSON.stringify(m.extensionKind)}`)
  if (m.extensionDependencies !== undefined) errs.push('extensionDependencies is declared')
  if (m.extensionPack !== undefined) errs.push('extensionPack is declared')
  const act: string[] = m.activationEvents ?? []
  if (act.includes('*')) errs.push('activationEvents has *')
  if (act.includes('onStartupFinished')) errs.push('activationEvents has onStartupFinished, which waits for drydock.watch (E2)')
  for (const v of m.contributes?.views ? Object.values(m.contributes.views).flat() as Array<Record<string, string>> : []) {
    if (v.type === 'webview') errs.push(`view ${v.id} is a webview`)
  }
  if (m.contributes?.customEditors !== undefined) errs.push('customEditors is declared')
  return errs
}

function settingErrors(m: Manifest): string[] {
  const errs: string[] = []
  const props: Record<string, Record<string, unknown>> = m.contributes?.configuration?.properties ?? {}
  for (const [k, v] of Object.entries(props)) {
    if (!k.startsWith('drydock.')) errs.push(`setting ${k} is not drydock.*`)
    if (v.scope !== 'application') errs.push(`setting ${k} has scope ${String(v.scope)}`)
    if (v.ignoreSync !== true) errs.push(`setting ${k} is synced`)
  }
  return errs
}

function engineErrors(m: Manifest, types: string): string[] {
  const minor = /^(\d+)\.(\d+)\./.exec(types)
  const want = minor === null ? '' : `^${minor[1]}.${minor[2]}.0`
  return m.engines?.vscode === want ? [] : [`engines.vscode is ${m.engines?.vscode}, @types/vscode is ${types} (want ${want})`]
}

function rangeErrors(m: Manifest): string[] {
  const r = m.drydock?.apiRange
  return Array.isArray(r) && r.length === 2 && Number.isInteger(r[0]) && Number.isInteger(r[1]) && r[0] >= 1 && r[0] <= r[1]
    ? [] : [`drydock.apiRange is ${JSON.stringify(r)}`]
}

const clone = (): Manifest => JSON.parse(JSON.stringify(real))

describe('the manifest', () => {
  it('contributes exactly the registry, every command drydock.* in the Drydock category and in the palette', () => {
    expect(COMMANDS.length).toBeGreaterThan(0)
    expect(commandErrors(real)).toEqual([])
  })

  it('a missing, stray, miscategorised or hidden command fails by name (control)', () => {
    const m = clone()
    m.contributes.commands = m.contributes.commands.filter((c: any) => c.command !== 'drydock.signOut')
    m.contributes.commands.push({ command: 'drydock.stray', title: 'Stray', category: 'Drydock' })
    m.contributes.commands[0].category = 'Other'
    m.contributes.menus = { commandPalette: [{ command: 'drydock.showLog', when: 'false' }] }
    const errs = commandErrors(m).join('\n')
    expect(errs).toContain('drydock.signOut is registered but not contributed')
    expect(errs).toContain('drydock.stray is contributed but never registered')
    expect(errs).toContain('has category Other')
    expect(errs).toContain('drydock.showLog is hidden from the palette')
  })

  it('runs on the laptop only, depends on nothing, activates lazily, and has no webview', () => {
    expect(shapeErrors(real)).toEqual([])
    const m = clone()
    m.extensionKind = ['workspace']
    m.extensionDependencies = ['ms-vscode-remote.remote-ssh']
    m.activationEvents = ['*']
    m.contributes.views = { drydock: [{ id: 'drydock.web', type: 'webview' }] }
    expect(shapeErrors(m)).toEqual([
      'extensionKind is ["workspace"]', 'extensionDependencies is declared', 'activationEvents has *', 'view drydock.web is a webview',
    ])
  })

  it('every setting is application-scoped and not synced (§6.4)', () => {
    expect(Object.keys(real.contributes.configuration.properties)).toContain('drydock.url')
    expect(settingErrors(real)).toEqual([])
    const m = clone()
    m.contributes.configuration.properties['drydock.url'].scope = 'machine'
    delete m.contributes.configuration.properties['drydock.connectWhenReady'].ignoreSync
    expect(settingErrors(m)).toEqual(['setting drydock.url has scope machine', 'setting drydock.connectWhenReady is synced'])
  })

  it('engines.vscode is the minor the types are for, and the api range is sane', () => {
    expect(engineErrors(real, typesVersion)).toEqual([])
    expect(engineErrors(real, '1.200.0')).toHaveLength(1)
    expect(rangeErrors(real)).toEqual([])
    expect(rangeErrors({ drydock: { apiRange: [2, 1] } })).toHaveLength(1)
  })
})
