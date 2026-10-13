// The extension's boundaries as named rules (vscode-extension §12.2, §8 rule
// 5, §12.8), beside the compiler's (src/tsconfig.pure.json):
//
//   - only src/adapter imports `vscode`;
//   - the pure layer (everything but src/adapter and src/node) imports only
//     relative paths, and only into src/, the extension's two JSON files, or
//     web/src/core — never the web app around core, never a package or a
//     `node:` module;
//   - src/node imports nothing but relative paths (Node's globals are its
//     reason to exist);
//   - nowhere in the extension's sources: a way to turn TLS validation off,
//     a child process (the extension spawns nothing, so there is no argv
//     sink to sweep), a webview, or a triple-slash directive (which would
//     put ambient types back past the compiler's rule).
//
// Specs are held to the text rules; their imports are not (they read Go
// source with node:fs).

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve, sep } from 'node:path'
import { describe, expect, it } from 'vitest'

const ROOT = resolve(process.cwd())
const SRC = join(ROOT, 'src')
const CORE = resolve(ROOT, '../web/src/core')

function files(dir: string, ext = /\.(ts|mts|mjs)$/): string[] {
  return readdirSync(dir).flatMap((n) => {
    const p = join(dir, n)
    if (statSync(p).isDirectory()) return files(p, ext)
    return ext.test(n) ? [p] : []
  })
}

export function specifiers(src: string): string[] {
  const out: string[] = []
  for (const re of [
    /\b(?:import|export)\b[^'"`;]*?\bfrom\s*(['"])([^'"]+)\1/g,
    /\bimport\s*(['"])([^'"]+)\1/g,
    /\bimport\s*\(\s*(['"`])([^'"`]+)\1/g,
    /\brequire\s*\(\s*(['"`])([^'"`]+)\1/g,
  ]) {
    for (const m of src.matchAll(re)) out.push(m[2]!)
  }
  return out
}

type Layer = 'adapter' | 'node' | 'pure'

export function layerOf(file: string): Layer {
  const rel = relative(SRC, file).split(sep)
  return rel[0] === 'adapter' ? 'adapter' : rel[0] === 'node' ? 'node' : 'pure'
}

/** Why `spec`, imported by `file`, breaks the boundary, or null. */
export function importViolation(file: string, spec: string): string | null {
  const layer = layerOf(file)
  if (spec === 'vscode') return layer === 'adapter' ? null : 'imports vscode outside src/adapter'
  if (!spec.startsWith('./') && !spec.startsWith('../')) return `imports ${spec}, which is not a relative path`
  if (layer !== 'pure') return null
  const target = resolve(dirname(file), spec)
  const inSrc = !relative(SRC, target).startsWith('..')
  const inCore = !relative(CORE, target).startsWith('..')
  const json = target === join(ROOT, 'package.json') || target === join(ROOT, 'contract.json')
  const inLayer = inSrc && !['adapter', 'node'].includes(relative(SRC, target).split(sep)[0]!)
  if (inLayer || inCore || json) return null
  return `imports ${spec}, which is outside the pure layer, web/src/core and the extension's JSON`
}

// Spelled in pieces so this file does not trip its own rule.
const BANNED: ReadonlyArray<[RegExp, string]> = [
  [new RegExp('reject' + 'Unauthorized'), 'turns TLS validation off'],
  [new RegExp('NODE_TLS_' + 'REJECT_UNAUTHORIZED'), 'turns TLS validation off'],
  [new RegExp('child' + '_process'), 'starts a process'],
  [new RegExp('createWebview' + 'Panel|registerWebview' + 'ViewProvider|registerCustom' + 'EditorProvider'), 'opens a webview'],
  [/^\s*\/\/\/\s*</m, 'carries a triple-slash directive'],
]

export function textViolations(src: string): string[] {
  return BANNED.filter(([re]) => re.test(src)).map(([, why]) => why)
}

describe('extension boundary', () => {
  it('every source file imports only what its layer may', () => {
    const all = files(SRC).filter((f) => !f.endsWith('.spec.ts'))
    const bad: string[] = []
    const vscodeImports: string[] = []
    for (const f of all) {
      for (const s of specifiers(readFileSync(f, 'utf8'))) {
        if (s === 'vscode') vscodeImports.push(relative(SRC, f))
        const why = importViolation(f, s)
        if (why !== null) bad.push(`${relative(ROOT, f)}: ${why}`)
      }
    }
    // The control: the scan saw the three layers, and found the adapter's
    // own vscode import, so an empty failure list is not a vacuous one.
    expect(all.map(layerOf)).toEqual(expect.arrayContaining(['adapter', 'node', 'pure']))
    expect(vscodeImports).toContain(join('adapter', 'extension.ts'))
    expect(bad).toEqual([])
  })

  it('nothing in the extension turns TLS off, spawns, opens a webview or carries a directive', () => {
    // The test harnesses start processes (the stack, VS Code, tar): only
    // the extension's own sources are held to "spawns nothing".
    const all = [...files(SRC), ...files(join(ROOT, 'test')), ...files(join(ROOT, 'scripts'))]
    expect(all.length).toBeGreaterThan(10)
    const bad = all.flatMap((f) => textViolations(readFileSync(f, 'utf8'))
      .filter((w) => w !== 'starts a process' || f.startsWith(SRC + sep))
      .map((w) => `${relative(ROOT, f)}: ${w}`))
    expect(bad).toEqual([])
  })

  it('each rule fires on what it is about, and passes what it is not (control)', () => {
    const pure = join(SRC, 'link.ts')
    const adapter = join(SRC, 'adapter', 'extension.ts')
    const node = join(SRC, 'node', 'transport.ts')
    expect(importViolation(pure, 'vscode')).toContain('outside src/adapter')
    expect(importViolation(node, 'vscode')).toContain('outside src/adapter')
    expect(importViolation(adapter, 'vscode')).toBeNull()
    expect(importViolation(pure, 'node:fs')).toContain('not a relative path')
    expect(importViolation(pure, '../../web/src/stores/stream')).toContain('outside')
    expect(importViolation(pure, './adapter/extension')).toContain('outside')
    expect(importViolation(pure, '../../web/src/core/reducer')).toBeNull()
    expect(importViolation(pure, '../contract.json')).toBeNull()
    expect(importViolation(adapter, '../client')).toBeNull()
    expect(textViolations('const a = { ' + 'reject' + 'Unauthorized: false }')).toEqual(['turns TLS validation off'])
    expect(textViolations("import { spawn } from 'node:" + 'child' + "_process'")).toEqual(['starts a process'])
    expect(textViolations('/// <reference lib="dom" />\nexport {}')).toEqual(['carries a triple-slash directive'])
    expect(textViolations(readFileSync(pure, 'utf8'))).toEqual([])
  })
})
