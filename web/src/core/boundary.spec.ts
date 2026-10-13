// The core boundary (vscode-extension §5.2), as a named rule beside the
// compiler's (core/tsconfig.json): every module in core/ imports only other
// modules in core/, by relative path. No package (vue, pinia), no `node:`
// module, nothing from the web app around it — so the extension can bundle
// core by path and get exactly the code the web UI runs.
//
// Specs are exempt: they run under Vitest and may read Go source (node:fs) or
// drive the mock backend. Everything else in core/ is held to it, fixtures
// included.

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const CORE = resolve(process.cwd(), 'src/core')

function modules(dir: string): string[] {
  return readdirSync(dir).flatMap((n) => {
    const p = join(dir, n)
    if (statSync(p).isDirectory()) return modules(p)
    return n.endsWith('.ts') && !n.endsWith('.spec.ts') ? [p] : []
  })
}

/** Every module specifier in `src`: static imports, re-exports, side-effect imports, dynamic import() and require(). */
export function specifiers(src: string): string[] {
  const out: string[] = []
  const patterns = [
    /\b(?:import|export)\b[^'"`;]*?\bfrom\s*(['"])([^'"]+)\1/g,
    /\bimport\s*(['"])([^'"]+)\1/g,
    /\bimport\s*\(\s*(['"`])([^'"`]+)\1/g,
    /\brequire\s*\(\s*(['"`])([^'"`]+)\1/g,
  ]
  for (const re of patterns) {
    for (const m of src.matchAll(re)) out.push(m[2]!)
  }
  return out
}

/**
 * Each triple-slash directive in `src` (`/// <reference lib="dom" />`,
 * `types=`, `path=`, `no-default-lib`): any one would let a core module pull
 * the DOM, Node or a package's types back in past core/tsconfig.json, so core
 * has none.
 */
export function directives(src: string): string[] {
  return [...src.matchAll(/^\s*\/\/\/\s*<[^\n]*/gm)].map((m) => m[0].trim())
}

/** Why `spec`, imported by `file`, breaks the boundary, or null when it is inside core. */
export function violation(file: string, spec: string): string | null {
  if (!spec.startsWith('./') && !spec.startsWith('../')) return `imports ${spec}, which is not a relative path inside core/`
  const target = resolve(dirname(file), spec)
  const rel = relative(CORE, target)
  if (rel.startsWith('..')) return `imports ${spec}, which is outside core/`
  return null
}

describe('core boundary', () => {
  it('every core module imports only core modules', () => {
    const files = modules(CORE)
    const seen: string[] = []
    const bad: string[] = []
    for (const f of files) {
      for (const spec of specifiers(readFileSync(f, 'utf8'))) {
        seen.push(spec)
        const why = violation(f, spec)
        if (why !== null) bad.push(`${relative(CORE, f)}: ${why}`)
      }
    }
    // The control: the scan saw core's modules and their imports — the
    // reducer's own import of the types among them — so an empty pass is not
    // a vacuous one.
    expect(files.map((f) => relative(CORE, f))).toContain('reducer.ts')
    expect(seen).toContain('./types')
    expect(bad).toEqual([])
  })

  it('no core module carries a triple-slash directive', () => {
    const files = modules(CORE)
    const bad = files.flatMap((f) => directives(readFileSync(f, 'utf8')).map((d) => `${relative(CORE, f)}: ${d}`))
    expect(files.length).toBeGreaterThan(0)
    expect(bad).toEqual([])
  })

  it('finds each directive form, and none in a clean module (control)', () => {
    expect(directives('/// <reference lib="dom" />\nexport const w = window')).toEqual(['/// <reference lib="dom" />'])
    expect(directives('  ///<reference types="node"/>')).toEqual(['///<reference types="node"/>'])
    expect(directives('/// <reference path="../x.d.ts" />')).toHaveLength(1)
    expect(directives('/// <amd-module name="x" />')).toHaveLength(1)
    // A clean module, and the comments core really uses, pass.
    expect(directives(readFileSync(join(CORE, 'marks.ts'), 'utf8'))).toEqual([])
    expect(directives('// a comment\n/** a doc */\nexport const x = 1 // trailing /// <ref')).toEqual([])
  })

  it('names each kind of import that breaks it, and passes one that does not (control)', () => {
    const f = join(CORE, 'reducer.ts')
    expect(specifiers("import { ref } from 'vue'")).toEqual(['vue'])
    expect(violation(f, 'vue')).toContain('not a relative path')
    expect(violation(f, 'node:fs')).toContain('not a relative path')
    expect(violation(f, '../stores/stream')).toContain('outside core/')
    expect(specifiers("export { x } from '../api/client'\nimport './side'\nconst m = import('./lazy')")).toEqual(
      ['../api/client', './side', './lazy'])
    expect(violation(join(CORE, 'lib/approval.ts'), '../types')).toBeNull()
  })
})
