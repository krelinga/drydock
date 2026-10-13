// The ritual probe: a stand-in for a hostile extension a repository put in
// its container (vscode-extension §2.3), for Spike 05's laptop measurements
// #2, #3, #4 and #5 (test/rituals/vscode-connect.md, part 2). It is a
// `workspace` extension, so in a container window it runs in the container's
// remote extension host, which is where a hostile one would run.
//
// It is safe to have run by mistake, and refuses to run where it should not:
// - every command but *Where Am I?* does nothing unless this host is in a
//   container (a container window's remoteName, Linux, /.dockerenv);
// - it reads no file, setting, secret or environment variable beyond those
//   three facts and the hostname, and sends nothing over the network;
// - the one Drydock command it calls is drydock.refreshRepositories, a
//   catalog refresh, harmless wherever it runs;
// - the text it types into terminals is `echo <marker>; hostname`.
// What each command did is written to its output channel, *Drydock Ritual
// Probe*, which the ritual copies into the spike report.

'use strict'
const vscode = require('vscode')
const fs = require('fs')
const os = require('os')

const CONTAINER_WINDOWS = new Set(['attached-container', 'dev-container'])

let out
const log = (line) => out.appendLine(`${new Date().toISOString()}  ${line}`)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

function where() {
  return {
    remoteName: vscode.env.remoteName,
    platform: process.platform,
    hostname: os.hostname(),
    dockerenv: fs.existsSync('/.dockerenv'),
  }
}

function inContainer() {
  const w = where()
  return CONTAINER_WINDOWS.has(w.remoteName) && w.platform === 'linux' && w.dockerenv
}

async function refuseOutsideContainer() {
  if (inContainer()) return false
  log(`refused: not in a container (${JSON.stringify(where())})`)
  out.show(true)
  await vscode.window.showErrorMessage(
    'The Drydock ritual probe is not running in a container. Uninstall it now (test/rituals/vscode-connect.md, "Removing the probe").')
  return true
}

/** Runs command `id` with `args`, settled or not within `ms`; never throws. */
async function attempt(id, args, ms = 2000) {
  const ran = vscode.commands.executeCommand(id, ...args).then(
    (v) => `ran, returned ${v === undefined ? 'undefined' : JSON.stringify(v)}`,
    (e) => `refused: ${e && e.message ? e.message : String(e)}`)
  const late = sleep(ms).then(() => `no answer within ${ms} ms`)
  return Promise.race([ran, late])
}

// #4: commands that might accept whatever has focus. A command that does not
// exist is refused, which is recorded and harmless.
const ACCEPTORS = [
  ['workbench.action.acceptSelectedQuickOpenItem', []],
  ['notification.acceptPrimaryAction', []],
  ['workbench.action.quickInputAccept', []],
  ['workbench.action.dialog.accept', []],
  ['type', [{ text: '\n' }]],
]

function activate(context) {
  out = vscode.window.createOutputChannel('Drydock Ritual Probe')
  context.subscriptions.push(out)
  const reg = (id, fn) => context.subscriptions.push(vscode.commands.registerCommand(id, fn))

  reg('drydockRitual.whereAmI', async () => {
    const w = where()
    log(`where: ${JSON.stringify(w)}; in a container: ${inContainer()}`)
    out.show(true)
    const verdict = inContainer() ? 'in a container: go on.' : 'NOT in a container: uninstall it now.'
    await vscode.window.showInformationMessage(`Probe: remoteName ${w.remoteName}, host ${w.hostname}; ${verdict}`)
  })

  reg('drydockRitual.callMutation', async () => {
    if (await refuseOutsideContainer()) return
    log(`#2 calling drydock.refreshRepositories from ${JSON.stringify(where())}`)
    log(`#2 drydock.refreshRepositories: ${await attempt('drydock.refreshRepositories', [], 60000)}`)
    out.show(true)
  })

  reg('drydockRitual.acceptModal', async () => {
    if (await refuseOutsideContainer()) return
    log(`#4 window.dialogStyle (as this host reads it): ${vscode.workspace.getConfiguration('window').get('dialogStyle')}`)
    let settled = null
    const shown = vscode.window.showWarningMessage(
      'Drydock ritual #4: press nothing until the probe says so.',
      { modal: true, detail: 'The probe now tries to accept this dialog from the container. If it closes by itself, a command accepted it.' },
      'Accepted by the probe')
    shown.then((v) => { settled = v === undefined ? 'dismissed' : v })
    await sleep(3000)
    for (const [id, args] of ACCEPTORS) {
      log(`#4 ${id}: ${await attempt(id, args)}`)
      await sleep(500)
      if (settled !== null) break
    }
    await sleep(2000)
    if (settled === null) {
      log('#4 RESULT: the dialog is still open after every attempt (the belief holds). Close it yourself now.')
      out.show(true)
      const closed = await shown
      log(`#4 then closed by the operator: ${closed === undefined ? 'dismissed' : closed}`)
    } else {
      log(`#4 RESULT: the dialog closed with no key pressed: ${settled} (the belief is WRONG)`)
    }
    const names = (await vscode.commands.getCommands(true)).filter((c) => /accept|confirm|dialog/i.test(c)).sort()
    log(`#4 for the record, commands whose names say accept, confirm or dialog (not run): ${names.join(', ')}`)
    out.show(true)
  })

  reg('drydockRitual.localTerminal', async () => {
    if (await refuseOutsideContainer()) return
    // The control first: a terminal in the container, which a workspace
    // extension may plainly open.
    log(`#5 control, workbench.action.terminal.new: ${await attempt('workbench.action.terminal.new', [])}`)
    await sleep(1500)
    log(`#5 control, sendSequence: ${await attempt('workbench.action.terminal.sendSequence', [{ text: 'echo DRYDOCK-RITUAL-5-CONTROL; hostname\r' }])}`)
    await sleep(1500)
    log(`#5 workbench.action.terminal.newLocal: ${await attempt('workbench.action.terminal.newLocal', [])}`)
    await sleep(2500)
    log(`#5 sendSequence: ${await attempt('workbench.action.terminal.sendSequence', [{ text: 'echo DRYDOCK-RITUAL-5-LOCAL; hostname\r' }])}`)
    log(`#5 container hostname, for comparison: ${os.hostname()}`)
    out.show(true)
  })
}

function deactivate() {}

module.exports = { activate, deactivate }
