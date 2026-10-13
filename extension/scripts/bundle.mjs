// Builds the extension's one file, dist/extension.js, with esbuild: the
// adapter, the pure layer and web/src/core (by path, vscode-extension §5.2)
// in one CommonJS bundle, `vscode` left to the host. The bundle has a size
// budget, so a dependency pulled in by accident is noticed.

import { build } from 'esbuild'
import { statSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('..', import.meta.url))
const out = `${root}dist/extension.js`
const BUDGET = 200 * 1024

await build({
  entryPoints: [`${root}src/adapter/extension.ts`],
  outfile: out,
  bundle: true,
  platform: 'node',
  format: 'cjs',
  target: 'node22',
  external: ['vscode'],
  sourcemap: false,
  minify: false,
  legalComments: 'none',
  logLevel: 'warning',
})

const size = statSync(out).size
console.log(`dist/extension.js: ${(size / 1024).toFixed(1)} KB of a ${BUDGET / 1024} KB budget`)
if (size > BUDGET) {
  console.error('FAIL: the bundle is over its budget')
  process.exit(1)
}
