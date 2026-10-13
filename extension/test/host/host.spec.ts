// The extension-host tier's driver (vscode-extension §12.5): the client
// tier's stack, the pinned VS Code (checked by SHA-256), a run root holding
// its user data, extensions and logs, and the specs in inner/suite.ts run
// inside it by @vscode/test-electron, headless under xvfb (run.sh).
//
// Both runs use the packaged artifact (`npm run package`, the bytes the
// release uploads): the Test-mode run unpacks dist/drydock-vscode.vsix and
// develops *that* directory, so the bundle under test is the released one
// with the seam; the Production-mode run installs the .vsix like any user,
// and asserts it exports nothing.

import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
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

const VSIX = path.join(EXT, 'dist', 'drydock-vscode.vsix')

/** The .vsix unpacked into `dir`: its extension/ is what the Test-mode run develops. */
function unpack(dir: string): string {
  if (!existsSync(VSIX)) throw new Error(`no ${VSIX}: run.sh host packages it (npm run package)`)
  mkdirSync(dir, { recursive: true })
  execFileSync('unzip', ['-q', '-o', VSIX, '-d', dir])
  return path.join(dir, 'extension')
}

function report(out: string): string {
  try {
    const r = JSON.parse(readFileSync(out, 'utf8')) as { results: Array<{ name: string; ok: boolean; error?: string }> }
    return r.results.map((x) => `${x.ok ? '✓' : '✗'} ${x.name}${x.error ? `\n${x.error}` : ''}`).join('\n')
  } catch {
    return '(no results written)'
  }
}

it('in Production mode, the installed .vsix is lazy, registers every command, and exports nothing', async () => {
  const exe = await vscodeExecutable()
  const run = path.join(stack.root, 'vscode-production')
  const extensions = path.join(run, 'extensions')
  const user = path.join(run, 'user-data')
  mkdirSync(path.join(user, 'User'), { recursive: true })
  writeFileSync(path.join(user, 'User', 'settings.json'), JSON.stringify({ 'workbench.startupEditor': 'none' }))
  // Installed as a user installs it: the CLI, into this run's extensions directory.
  execFileSync(path.join(path.dirname(exe), 'bin', 'code'), [
    '--install-extension', VSIX, '--extensions-dir', extensions, '--user-data-dir', user, '--force',
  ], {
    stdio: 'inherit',
    // Not through a VS Code this test happens to run inside (a remote
    // terminal's VSCODE_IPC_HOOK_CLI): the pinned one, on its own.
    env: Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith('VSCODE_'))),
  })
  const suite = path.join(run, 'production.js')
  await build({
    entryPoints: [path.join(EXT, 'test', 'host', 'inner', 'production.ts')], outfile: suite, bundle: true,
    platform: 'node', format: 'cjs', target: 'node22', external: ['vscode'], logLevel: 'warning',
  })
  const out = path.join(run, 'results.json')
  const code = await runTests({
    vscodeExecutablePath: exe,
    // The probe is the extension in development; Drydock is installed.
    extensionDevelopmentPath: path.join(EXT, 'test', 'host', 'probe'),
    extensionTestsPath: suite,
    launchArgs: ['--user-data-dir', user, '--extensions-dir', extensions, '--logsPath', path.join(run, 'logs'),
      '--password-store=basic', '--disable-gpu', '--no-sandbox'],
    extensionTestsEnv: { DRYDOCK_HOST_OUT: out },
  }).catch((e: unknown) => {
    console.error(e)
    return 1
  })
  expect(code, report(out)).toBe(0)
}, 300_000)

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
    // The .vsix's own bytes, unpacked, in Test mode.
    extensionDevelopmentPath: unpack(path.join(run, 'unpacked')),
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
  if (code !== 0) {
    // What the stack saw, for a failure in CI.
    const tail = (f: string) => {
      try {
        return readFileSync(f, 'utf8').split('\n').slice(-40).join('\n')
      } catch {
        return ''
      }
    }
    console.error(`stack.log (tail):\n${tail(path.join(stack.root, 'stack.log'))}`)
  }
  expect(code, report).toBe(0)
}, 900_000)
