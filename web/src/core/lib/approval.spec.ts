// core's host-access request (vscode-extension §10.4, §12.2): the root
// sentences for each mix of privileged and initializeCommand, the groups, and
// repository-written text shown escaped. Both the web card
// (views/HostAccessApproval.spec.ts) and the extension's review document
// render this, so this is where what they say is pinned.

import { describe, expect, it } from 'vitest'
import type { ApprovalView, HostSettingView } from '../types'
import {
  INITIALIZE_COMMAND_SENTENCE, PRIVILEGED_SENTENCE, approvalSections, sourceLabel,
} from './approval'
import { escapeFormat, showValue } from './text'

const HASH = 'sha256:' + 'a'.repeat(64)
const req = (p: Partial<ApprovalView>): ApprovalView => ({ hash: HASH, added: [], changed: [], removed: [], ...p })
const priv: HostSettingView = { field: 'privileged', source: 'feature_or_image', value: true }
const init: HostSettingView = { field: 'initializeCommand', source: 'repository', value: 'make host-setup' }
const args: HostSettingView = { field: 'runArgs', source: 'repository', value: ['--network=host'] }

describe('approvalSections: the root sentences', () => {
  it('privileged new: its sentence alone', () => {
    const s = approvalSections(req({ added: [priv, args] }))
    expect(s.roots).toEqual([PRIVILEGED_SENTENCE])
    expect(s.privileged).toBe(true)
    expect(s.initializeCommand).toBe(false)
  })

  it('initializeCommand new: its sentence alone', () => {
    expect(approvalSections(req({ added: [init] })).roots).toEqual([INITIALIZE_COMMAND_SENTENCE])
  })

  it('both, one new and one changed: both sentences, privileged first', () => {
    const s = approvalSections(req({
      added: [init], changed: [{ field: 'privileged', source: 'repository', from: false, to: true }],
    }))
    expect(s.roots).toEqual([PRIVILEGED_SENTENCE, INITIALIZE_COMMAND_SENTENCE])
  })

  it('only removed earns no sentence; a plain request has none (control: the same fields added do)', () => {
    expect(approvalSections(req({ removed: [priv, init] })).roots).toEqual([])
    expect(approvalSections(req({ added: [args] })).roots).toEqual([])
    expect(approvalSections(req({ added: [priv, init] })).roots).toHaveLength(2)
  })

  it('the sentences say what each is', () => {
    expect(PRIVILEGED_SENTENCE).toContain('root on the host')
    expect(INITIALIZE_COMMAND_SENTENCE).toContain('runs on this server, not in the container')
    expect(INITIALIZE_COMMAND_SENTENCE).toContain("can read Drydock's keys and is root through Docker")
  })
})

describe('approvalSections: the groups', () => {
  it('new, changed and removed, in that order, each with its lines', () => {
    const s = approvalSections(req({
      added: [args],
      changed: [{ field: 'mounts', source: 'repository', from: ['a'], to: ['a', 'b'] }],
      removed: [priv],
    }))
    expect(s.groups.map((g) => [g.kind, g.label])).toEqual([
      ['added', 'New'], ['changed', 'Changed since the last approval'], ['removed', 'No longer asked for'],
    ])
    expect(s.groups[0]!.items[0]).toMatchObject({
      field: 'runArgs', source: "from the repository's devcontainer.json", values: ['["--network=host"]'],
    })
    expect(s.groups[1]!.items[0]!.values).toEqual(['was ["a"]', 'now ["a","b"]'])
    expect(s.groups[2]!.items[0]).toMatchObject({ field: 'privileged', source: 'from a Feature or the image', values: [] })
  })

  it('an empty group is left out', () => {
    expect(approvalSections(req({ added: [args] })).groups.map((g) => g.kind)).toEqual(['added'])
  })
})

describe('repository-written text is shown, never interpreted', () => {
  const RLO = '‮'
  const ZWSP = '​'

  it('a value with a newline, a control character or a bidi override renders as escaped text', () => {
    const s = approvalSections(req({
      added: [
        { field: 'initializeCommand', source: 'repository', value: 'echo ok\nprivileged: false' },
        { field: 'runArgs', source: 'repository', value: ['--x\u0007', `abc${RLO}fed`] },
      ],
    }))
    const [nl, ctl] = s.groups[0]!.items
    expect(nl!.values).toEqual(['"echo ok\\nprivileged: false"'])
    expect(ctl!.values).toEqual(['["--x\\u0007","abc\\u202Efed"]'])
    for (const line of [...nl!.values, ...ctl!.values]) expect(line).not.toMatch(/[\u0000-\u001f‮]/)
  })

  it('a field and an unknown source with format characters are escaped too', () => {
    const s = approvalSections(req({ added: [{ field: `privi${ZWSP}leged`, source: `feat${RLO}x`, value: true }] }))
    const it0 = s.groups[0]!.items[0]!
    expect(it0.field).toBe('privi\\u200Bleged')
    expect(it0.source).toBe('from feat\\u202Ex')
    // A look-alike field is not `privileged`, and is shown as visibly different.
    expect(s.privileged).toBe(false)
  })

  it('plain ASCII, accents and emoji render unchanged (control)', () => {
    expect(escapeFormat('krelinga/alpha')).toBe('krelinga/alpha')
    expect(escapeFormat('feature/café-🚀')).toBe('feature/café-🚀')
    expect(showValue(['--network=host'])).toBe('["--network=host"]')
    expect(sourceLabel('repository')).toBe("the repository's devcontainer.json")
  })

  it('escapes the separators, C1 controls, BOM and astral format characters', () => {
    expect(escapeFormat('a b c')).toBe('a\\u2028b\\u2029c')
    expect(escapeFormat('a\u0085b﻿c')).toBe('a\\u0085b\\uFEFFc')
    expect(escapeFormat('x\u{E0041}y')).toBe('x\\u{E0041}y')
    expect(escapeFormat('⁦iso⁩')).toBe('\\u2066iso\\u2069')
  })

  it('a value the server left out shows as nothing', () => {
    expect(showValue(undefined)).toBe('')
  })
})
