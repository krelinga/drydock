// The Production-mode run (vscode-extension §12.5): the released .vsix,
// installed into the extensions directory like any user's, with no test
// seam. It runs from a probe extension (test/host/probe), the only extension
// in development here, so Drydock's own runs in ExtensionMode.Production. It
// asserts what Production adds: the extension is there, lazy, registers
// every command once activated, and exports nothing — the seam cannot ship
// live. Nothing here signs in: without the seam, nothing can paste a token.

import { strict as assert } from 'node:assert'
import { writeFileSync } from 'node:fs'
import * as vscode from 'vscode'
import { COMMANDS } from '../../../src/commands'

const OUT = process.env.DRYDOCK_HOST_OUT ?? ''

export async function run(): Promise<void> {
  const results: Array<{ name: string; ok: boolean; error?: string }> = []
  const spec = async (name: string, fn: () => Promise<void>) => {
    try {
      await fn()
      results.push({ name, ok: true })
    } catch (e) {
      results.push({ name, ok: false, error: e instanceof Error ? `${e.message}\n${e.stack ?? ''}` : String(e) })
    }
  }
  const ext = vscode.extensions.getExtension('krelinga.drydock')

  await spec('the installed .vsix is the extension, not yet active', async () => {
    assert.ok(ext, 'krelinga.drydock is installed')
    assert.equal(ext.isActive, false)
    assert.equal(ext.extensionKind, vscode.ExtensionKind.UI)
  })

  await spec('activated by its own command, it registers every contributed command', async () => {
    await vscode.commands.executeCommand('drydock.showLog')
    assert.equal(ext?.isActive, true)
    const all = new Set(await vscode.commands.getCommands(true))
    assert.deepEqual(COMMANDS.map((c) => c.id).filter((id) => !all.has(id)), [])
  })

  await spec('in Production mode its exports are undefined: the test seam cannot ship live', async () => {
    assert.equal(ext?.exports, undefined)
  })

  if (OUT !== '') writeFileSync(OUT, JSON.stringify({ results }, null, 2))
  const failed = results.filter((r) => !r.ok)
  if (failed.length > 0) throw new Error(failed.map((f) => `✗ ${f.name}\n${f.error}`).join('\n\n'))
}
