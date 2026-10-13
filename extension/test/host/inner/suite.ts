// The extension-host tier's specs (vscode-extension §12.5), run inside the
// pinned VS Code's extension host by @vscode/test-electron, against the
// client tier's stack. The extension under test runs from its bundle in
// ExtensionMode.Test, where `activate` returns the seam: a scripted
// Prompter, a recording Opener and a Window reporting any remoteName. The
// stack, outside this process, is reached through the control channel
// (ctl.ts): what the tap saw, a minted token, a view's link.
//
// Specs run in order and share one window; each says what it leaves.
// Results, and Spike 05's measurements (§12.9), go to DRYDOCK_HOST_OUT.

import { strict as assert } from 'node:assert'
import { writeFileSync } from 'node:fs'
import * as vscode from 'vscode'
import type { Seam } from '../../../src/adapter/extension'
import { COMMANDS } from '../../../src/commands'
import { CONNECT } from '../../../src/flows'
import { PASTE_TOKEN } from '../../../src/session'
import { ELSEWHERE } from '../../../src/windowRule'
import { FakeWindow, RecordingOpener, ScriptedPrompter } from '../../fakes'

const CTL = process.env.DRYDOCK_CTL ?? ''
const OUT = process.env.DRYDOCK_HOST_OUT ?? ''
const ID = 'krelinga.drydock'

interface TapRecord { method: string; path: string; authorization: string | null; origin: string | null; status: number | null }

async function ctl<T>(method: string, path: string, body?: unknown): Promise<T> {
  const r = await fetch(CTL + path, { method, body: body === undefined ? undefined : JSON.stringify(body) })
  if (!r.ok) throw new Error(`ctl ${method} ${path}: ${r.status} ${await r.text()}`)
  return (await r.json()) as T
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

async function until(what: string, ok: () => boolean | Promise<boolean>, ms = 60_000): Promise<void> {
  const deadline = Date.now() + ms
  while (!(await ok())) {
    if (Date.now() > deadline) throw new Error(`${what}: never`)
    await sleep(100)
  }
}

const results: Array<{ name: string; ok: boolean; error?: string }> = []
const spike: Record<string, unknown> = {}

async function spec(name: string, fn: () => Promise<void>): Promise<void> {
  try {
    await fn()
    results.push({ name, ok: true })
  } catch (e) {
    results.push({ name, ok: false, error: e instanceof Error ? `${e.message}\n${e.stack ?? ''}` : String(e) })
  }
}

/** A Prompter answering every message with `answer`'s choice, every pick with `pick`'s, every paste with `typed`. */
function scripted(o: { answer?: (text: string, actions: readonly string[]) => string | undefined; pick?: (labels: string[]) => number | undefined; typed?: string[] } = {}): ScriptedPrompter {
  const p = new ScriptedPrompter()
  p.message = async function (level, text, actions = []) {
    this.shown.push({ level, text, actions })
    return o.answer?.(text, actions)
  }
  p.pick = async function <T>(items: readonly { label: string; value: T }[], opts: { placeHolder: string }) {
    this.picks.push({ placeHolder: opts.placeHolder, items: items.map((i) => ({ label: i.label, value: i.value })) })
    const n = o.pick?.(items.map((i) => i.label))
    return n === undefined ? undefined : items[n]?.value
  }
  p.typed = o.typed ?? []
  return p
}

export async function run(): Promise<void> {
  const origin = await ctl<string>('GET', '/origin')
  spike['1-env-in-extension-host'] = { NODE_EXTRA_CA_CERTS: process.env.NODE_EXTRA_CA_CERTS ?? null, systemCertificates: vscode.workspace.getConfiguration('http').get('systemCertificates') }
  const ext = vscode.extensions.getExtension<Seam>(ID)
  assert.ok(ext, 'the extension is installed')

  await spec('activation is lazy: nothing is asked of Drydock before a command runs', async () => {
    assert.equal(ext.isActive, false)
    await sleep(2_000)
    assert.deepEqual(await ctl<TapRecord[]>('GET', '/tap'), [])
  })

  let seam!: Seam
  await spec('activated, every contributed command is registered, and the Test-mode seam is returned', async () => {
    await vscode.commands.executeCommand('drydock.showLog')
    assert.equal(ext.isActive, true)
    seam = ext.exports
    assert.ok(seam !== undefined && typeof seam === 'object', 'Test mode returns the seam')
    const all = new Set(await vscode.commands.getCommands(true))
    const missing = COMMANDS.map((c) => c.id).filter((id) => !all.has(id))
    assert.deepEqual(missing, [])
    // Activation read the secret store and asked Drydock nothing.
    assert.deepEqual(await ctl<TapRecord[]>('GET', '/tap'), [])
  })

  await spec('workspace settings cannot move drydock.url; a workspace setting VS Code honours is the control', async () => {
    const cfg = vscode.workspace.getConfiguration('drydock')
    assert.equal(cfg.get('url'), origin)
    assert.equal(vscode.workspace.getConfiguration('editor').get('tabSize'), 7)
    spike['6-application-scope-workspace'] = { workspaceValue: cfg.inspect('url')?.workspaceValue ?? null, effective: cfg.get('url') }
  })

  await spec('http.proxyStrictSSL is not off in the test profile', async () => {
    assert.notEqual(vscode.workspace.getConfiguration('http').get('proxyStrictSSL'), false)
  })

  const { token } = await ctl<{ token: string }>('POST', '/mint', { scopes: ['read', 'lifecycle'], label: 'VS Code (host tier)' })
  const label = await ctl<string>('POST', '/label', { token })

  await spec('sign in: the pasted token is verified with GET /api/meta over HTTPS through Caddy, trusting the tier\'s CA', async () => {
    await ctl('POST', '/tap/clear')
    const p = scripted({ answer: (_t, a) => (a.includes(PASTE_TOKEN) ? PASTE_TOKEN : undefined), typed: [token] })
    seam.prompter = p
    await vscode.commands.executeCommand('drydock.signIn')
    const tap = await ctl<TapRecord[]>('GET', '/tap')
    assert.deepEqual(tap.map((t) => [t.method, t.path, t.authorization, t.status]), [['GET', '/api/meta', label, 200]])
    assert.ok(p.shown.some((s) => s.text.startsWith('Signed in to Drydock')), JSON.stringify(p.shown.map((s) => s.text)))
    spike['1-trust'] = { signedInOverHttps: true, how: 'the CA in the system store (test/extension/system-ca.sh), http.systemCertificates on' }
  })

  await spec('showing the Drydock view holds the stream: it opens, and the workspace list is read', async () => {
    await ctl('POST', '/tap/clear')
    await vscode.commands.executeCommand('workbench.view.extension.drydock')
    await until('the view read the list', async () => (await ctl<TapRecord[]>('GET', '/tap')).some((t) => t.path === '/api/workspaces'))
    const paths = (await ctl<TapRecord[]>('GET', '/tap')).map((t) => t.path)
    assert.ok(paths.includes('/api/events'), JSON.stringify(paths))
    await vscode.commands.executeCommand('workbench.view.explorer')
  })

  await spec('a certificate for another name is refused in the extension host too (the control for trust)', async () => {
    const wrong = await ctl<string>('POST', '/wrongcert')
    let err: unknown = null
    try {
      await fetch(`${wrong}/api/meta`)
    } catch (e) {
      err = e
    }
    const cause = (err as { cause?: { code?: string } } | null)?.cause?.code
    assert.equal(cause, 'ERR_TLS_CERT_ALTNAME_INVALID')
    spike['1-wrong-host-control'] = { refused: true, code: cause }
  })

  await spec('§8 rule 1 in a real host: with the window reporting attached-container, every mutating command sends nothing and says why', async () => {
    const p = scripted({ answer: (_t, a) => a[0], pick: () => 0, typed: [token] })
    p.dismissNow = true
    seam.prompter = p
    // Open in Drydock is the refusal's one action: recorded, not opened.
    seam.opener = new RecordingOpener()
    seam.window = new FakeWindow('attached-container')
    await ctl('POST', '/tap/clear')
    for (const c of COMMANDS.filter((x) => x.mutating)) await vscode.commands.executeCommand(c.id)
    const mutations = (await ctl<TapRecord[]>('GET', '/tap')).filter((t) => t.method !== 'GET')
    assert.deepEqual(mutations, [])
    assert.ok(p.shown.filter((s) => s.text === ELSEWHERE).length >= COMMANDS.filter((x) => x.mutating).length)
    // Control: the same refresh from a local window is sent.
    seam.window = new FakeWindow(undefined)
    await vscode.commands.executeCommand('drydock.refreshRepositories')
    const sent = (await ctl<TapRecord[]>('GET', '/tap')).filter((t) => t.method === 'POST')
    assert.deepEqual(sent.map((t) => [t.path, t.origin]), [['/api/repos/refresh', origin]])
  })

  await spec("E1's flow from the hub: clone, follow to running, Connect hands VS Code the vscode-remote form of the view's link", async () => {
    const opener = new RecordingOpener()
    seam.opener = opener
    seam.window = new FakeWindow(undefined)
    seam.extensions = { installed: () => true, install: async () => {} }
    const p = scripted({
      pick: (labels) => labels.findIndex((l) => l.endsWith('drydock-fixture/plain')),
      answer: (t, a) => (t.includes(' is running.') ? CONNECT : a.includes(CONNECT) ? CONNECT : undefined),
    })
    seam.prompter = p
    await ctl('POST', '/tap/clear')
    await vscode.commands.executeCommand('drydock.go')
    await until('a folder opened', () => opener.folders.length > 0)
    const links = await ctl<Record<string, string | null>>('GET', '/links')
    const link = links['drydock-fixture/plain']
    assert.ok(link, JSON.stringify(links))
    assert.deepEqual(opener.folders, [link.replace('vscode://vscode-remote/', 'vscode-remote://')])
    assert.ok(p.progresses.length >= 1, 'the follow was a progress notification')
    const creates = (await ctl<TapRecord[]>('GET', '/tap')).filter((t) => t.method === 'POST' && t.path === '/api/workspaces')
    assert.equal(creates.length, 1)
  })

  await spec("VS Code's default keybindings can be read, none uses the hub's chord, and ctrl+shift+p is found (the control)", async () => {
    let text: string | null = null
    let how = ''
    try {
      text = (await vscode.workspace.openTextDocument(vscode.Uri.parse('vscode://defaultsettings/keybindings.json'))).getText()
      how = 'openTextDocument(vscode://defaultsettings/keybindings.json)'
    } catch (e) {
      spike['7-open-text-document-error'] = String(e)
    }
    if (text === null || text.length === 0) {
      await vscode.commands.executeCommand('workbench.action.openDefaultKeybindingsFile')
      text = vscode.window.activeTextEditor?.document.getText() ?? null
      how = 'workbench.action.openDefaultKeybindingsFile, then the active editor'
    }
    assert.ok(text !== null && text.length > 1000, 'the default keybindings were read')
    const bindings = JSON.parse(text.replace(/^\s*\/\/.*$/gm, '')) as Array<{ key: string; command: string }>
    const norm = (k: string) => k.split('+').sort().join('+')
    const ours = norm('ctrl+alt+shift+d')
    const holders = bindings.filter((b) => b.key.split(' ').some((part) => norm(part) === ours))
    // The document lists extensions' bindings too: ours is the control that
    // the chord is found when it is there, and nothing else may hold it.
    assert.ok(holders.some((b) => b.command === 'drydock.go'), JSON.stringify(holders))
    assert.deepEqual(holders.filter((b) => !b.command.startsWith('drydock.')), [])
    assert.ok(bindings.some((b) => norm(b.key) === norm('ctrl+shift+p') && b.command === 'workbench.action.showCommands'))
    spike['7-default-keybindings'] = { readable: true, how, count: bindings.length }
  })

  if (OUT !== '') writeFileSync(OUT, JSON.stringify({ results, spike }, null, 2))
  const failed = results.filter((r) => !r.ok)
  if (failed.length > 0) throw new Error(failed.map((f) => `✗ ${f.name}\n${f.error}`).join('\n\n'))
}
