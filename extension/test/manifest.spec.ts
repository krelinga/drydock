// The manifest is a tested artifact (vscode-extension §12.3): most "does the
// palette and keyboard work" questions are about package.json, and need no
// editor. Each check is a function of a manifest, run against the real one
// and against a broken fixture that must fail it by name (the controls).

import { existsSync, readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { COMMANDS } from '../src/commands'

type Manifest = Record<string, any>
// The source's package.json, or — in the release's verify job — the one
// unpacked from the .vsix (DRYDOCK_MANIFEST).
const MANIFEST = process.env.DRYDOCK_MANIFEST ?? resolve(process.cwd(), 'package.json')
const real: Manifest = JSON.parse(readFileSync(MANIFEST, 'utf8'))
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

/** Context keys a `when` may use: the ones this extension sets (none yet) and VS Code documents for views. */
const WHEN_KEYS = new Set(['focusedView', 'view', 'viewItem', 'resourceScheme'])
/** Commands that must never be one keystroke away (§10.4): no keybinding, ever. */
const NEVER_BOUND = new Set(['drydock.approve', 'drydock.decline', 'drydock.signOut', 'drydock.clone'])

function keybindingErrors(m: Manifest): string[] {
  const errs: string[] = []
  const ids = new Set((m.contributes?.commands ?? []).map((c: { command: string }) => c.command))
  const kbs: Array<{ command: string; key?: string; mac?: string; when?: string }> = m.contributes?.keybindings ?? []
  for (const k of kbs) {
    if (!ids.has(k.command)) errs.push(`keybinding for ${k.command}, which is not contributed`)
    if (!k.key || !k.mac) errs.push(`keybinding for ${k.command} lacks a default or a mac key`)
    if (NEVER_BOUND.has(k.command)) errs.push(`keybinding for ${k.command}, which must never be one keystroke`)
    if (k.command === 'drydock.go' && k.when !== undefined) errs.push('the hub binding has a when clause: it must work in every window (§6.2)')
    for (const key of whenKeys(k.when)) {
      if (!knownKey(key)) errs.push(`keybinding for ${k.command} uses an unknown context key ${key}`)
    }
  }
  return errs
}

/** The context keys a `when` clause reads: the left side of each comparison, and each bare key. */
export function whenKeys(when: string | undefined): string[] {
  if (when === undefined) return []
  return when.split(/&&|\|\|/).map((c) => c.trim().replace(/^!\s*/, '').split(/\s*(?:==|!=|=~)\s*/)[0]!.trim()).filter((k) => k !== '')
}

const knownKey = (k: string) => WHEN_KEYS.has(k) || /^drydock\.[A-Za-z]+$/.test(k)

function menuErrors(m: Manifest): string[] {
  const errs: string[] = []
  const ids = new Set((m.contributes?.commands ?? []).map((c: { command: string }) => c.command))
  const views = new Set(Object.values(m.contributes?.views ?? {}).flat().map((v) => (v as { id: string }).id))
  for (const [menu, entries] of Object.entries(m.contributes?.menus ?? {})) {
    for (const e of entries as Array<{ command: string; when?: string }>) {
      if (!ids.has(e.command)) errs.push(`${menu}: ${e.command} is not contributed`)
      for (const key of whenKeys(e.when)) if (!knownKey(key)) errs.push(`${menu}: ${e.command} uses an unknown context key ${key}`)
      for (const v of (e.when ?? '').match(/view == ([\w.]+)/g) ?? []) {
        const id = v.replace('view == ', '')
        if (!views.has(id)) errs.push(`${menu}: ${e.command} names a view that is not contributed: ${id}`)
      }
    }
  }
  for (const w of (m.contributes?.viewsWelcome ?? []) as Array<{ view: string; when?: string }>) {
    if (!views.has(w.view)) errs.push(`viewsWelcome names a view that is not contributed: ${w.view}`)
    for (const key of whenKeys(w.when)) if (!knownKey(key)) errs.push(`viewsWelcome uses an unknown context key ${key}`)
  }
  for (const c of (m.contributes?.viewsContainers?.activitybar ?? []) as Array<{ icon: string }>) {
    if (!existsSync(resolve(dirname(MANIFEST), c.icon))) errs.push(`the activity bar icon ${c.icon} does not exist`)
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

  it('the one default keybinding is the hub, ctrl+alt+shift+d / cmd+alt+shift+d, with no when (§6.2)', () => {
    expect(real.contributes.keybindings).toEqual([{ command: 'drydock.go', key: 'ctrl+alt+shift+d', mac: 'cmd+alt+shift+d' }])
    expect(keybindingErrors(real)).toEqual([])
  })

  it('a binding to nothing, without a mac key, with a when on the hub, or on a command never bound fails by name (control)', () => {
    const m = clone()
    m.contributes.keybindings = [
      { command: 'drydock.nope', key: 'ctrl+x', mac: 'cmd+x' },
      { command: 'drydock.go', key: 'ctrl+alt+shift+d', when: 'editorFocus' },
      { command: 'drydock.signOut', key: 'ctrl+y', mac: 'cmd+y' },
    ]
    const errs = keybindingErrors(m).join('\n')
    expect(errs).toContain('drydock.nope, which is not contributed')
    expect(errs).toContain('lacks a default or a mac key')
    expect(errs).toContain('the hub binding has a when clause')
    expect(errs).toContain('unknown context key editorFocus')
    expect(errs).toContain('drydock.signOut, which must never be one keystroke')
  })

  it('the views, their welcome and their menus name only contributed commands and views, and known context keys', () => {
    expect(Object.keys(real.contributes.menus)).toContain('view/item/context')
    expect(menuErrors(real)).toEqual([])
    const m = clone()
    m.contributes.menus['view/item/context'].push({ command: 'drydock.nope', when: 'view == drydock.elsewhere && sideBarFocus' })
    m.contributes.viewsWelcome.push({ view: 'drydock.ghost', contents: 'x', when: 'editorFocus' })
    m.contributes.viewsContainers.activitybar[0].icon = 'media/missing.svg'
    const errs = menuErrors(m).join('\n')
    expect(errs).toContain('drydock.nope is not contributed')
    expect(errs).toContain('unknown context key sideBarFocus')
    expect(errs).toContain('names a view that is not contributed: drydock.elsewhere')
    expect(errs).toContain('viewsWelcome names a view that is not contributed: drydock.ghost')
    expect(errs).toContain('the activity bar icon media/missing.svg does not exist')
    expect(whenKeys('view == drydock.workspaces && viewItem =~ /^workspace\\./ && !drydock.signedIn')).toEqual(['view', 'viewItem', 'drydock.signedIn'])
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
