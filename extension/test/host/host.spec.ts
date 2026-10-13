// The extension-host tier's driver (vscode-extension §12.5): the client
// tier's stack, the pinned VS Code (checked by SHA-256), a run root holding
// its user data, extensions and logs, and the specs in inner/suite.ts run
// inside it by @vscode/test-electron, headless under xvfb (run.sh).
//
// The extension runs from extension/ with its bundle (`npm run bundle`) in
// ExtensionMode.Test. Running the bundle unpacked from the released .vsix,
// and a Production-mode run of the installed .vsix, come with the .vsix
// itself (PR 6).

import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { build } from 'esbuild'
import { runTests } from '@vscode/test-electron'
import { afterAll, beforeAll, expect, it } from 'vitest'
import { ExtensionStack } from '../../../test/extension/stack'
import { startCtl, stopOthers, type Ctl } from './ctl'
import { vscodeExecutable } from './vscode'

const EXT = process.cwd()
const REPO = path.resolve(EXT, '..')
let stack: ExtensionStack
let ctl: Ctl

beforeAll(async () => {
  const certs = process.env.DRYDOCK_EXTENSION_CERTS
  if (!certs) throw new Error('no DRYDOCK_EXTENSION_CERTS: run this tier with test/extension/run.sh host')
  stack = await ExtensionStack.start(REPO, certs)
  ctl = await startCtl(stack)
})
afterAll(async () => {
  await ctl?.close()
  await stopOthers()
  await stack?.teardown()
})

it('the extension-host specs pass in the pinned VS Code', async () => {
  const exe = await vscodeExecutable()
  const run = path.join(stack.root, 'vscode')
  const user = path.join(run, 'user-data')
  const folder = path.join(run, 'workspace')
  mkdirSync(path.join(user, 'User'), { recursive: true })
  mkdirSync(path.join(folder, '.vscode'), { recursive: true })
  // The operator's own settings: the stack's origin. The workspace's
  // settings try to move it (a repository can write them) and set a setting
  // VS Code does honour there, the control.
  writeFileSync(path.join(user, 'User', 'settings.json'), JSON.stringify({
    'drydock.url': stack.origin,
    'window.dialogStyle': 'custom',
    'workbench.startupEditor': 'none',
    'security.workspace.trust.enabled': false,
  }))
  writeFileSync(path.join(folder, '.vscode', 'settings.json'), JSON.stringify({ 'drydock.url': 'https://other.test', 'editor.tabSize': 7 }))

  const suite = path.join(run, 'suite.js')
  await build({
    entryPoints: [path.join(EXT, 'test', 'host', 'inner', 'index.ts')], outfile: suite, bundle: true,
    platform: 'node', format: 'cjs', target: 'node22', external: ['vscode'], logLevel: 'warning',
  })
  const out = path.join(run, 'results.json')
  const code = await runTests({
    vscodeExecutablePath: exe,
    extensionDevelopmentPath: EXT,
    extensionTestsPath: suite,
    launchArgs: [
      folder,
      '--user-data-dir', user,
      '--extensions-dir', path.join(run, 'extensions'),
      '--logsPath', path.join(run, 'logs'),
      '--password-store=basic',
      '--disable-gpu',
      '--no-sandbox',
    ],
    extensionTestsEnv: {
      DRYDOCK_CTL: ctl.url, DRYDOCK_HOST_OUT: out,
    },
  }).catch((e: unknown) => {
    console.error(e)
    return 1
  })
  let report = ''
  try {
    const r = JSON.parse(readFileSync(out, 'utf8')) as { results: Array<{ name: string; ok: boolean; error?: string }>; spike: unknown }
    report = r.results.map((x) => `${x.ok ? '✓' : '✗'} ${x.name}${x.error ? `\n${x.error}` : ''}`).join('\n')
    // Kept for Spike 05's report (docs/design/spikes/05-vscode-extension-host.md) when asked.
    if (process.env.DRYDOCK_HOST_RESULTS) writeFileSync(process.env.DRYDOCK_HOST_RESULTS, JSON.stringify(r, null, 2))
    console.log(`${report}\n\nSpike 05 measurements:\n${JSON.stringify(r.spike, null, 2)}`)
  } catch {
    report = '(no results written)'
  }
  expect(code, report).toBe(0)
}, 900_000)
