// The adapter (vscode-extension §12.2): the only code that imports `vscode`.
// It builds the ports from VS Code's API and registers every command in
// COMMANDS through the dispatcher, and does nothing else worth a branch: the
// logic is in the pure layer, where it is unit-tested.
//
// Activation is lazy: no activation events but the commands' own, so a
// window that never touches Drydock makes no request and reads no secret.

import * as vscode from 'vscode'
import { COMMANDS, type CommandId } from '../commands'
import { CredentialStore } from '../credential'
import { DrydockClient } from '../client'
import { Dispatcher, type Handler } from '../dispatch'
import { redacting } from '../log'
import type { Log, Opener, Prompter, Secrets, Settings, Window } from '../ports'
import { StaleTokenWatch, openInBrowser, signIn, signOut, type SessionDeps } from '../session'
import { originOf } from '../settings'
import { nodeTransport } from '../node/transport'

/** The one SecretStorage key: `{origin, token}` (credential.ts). */
const SECRET_KEY = 'drydock.credential'

export function activate(context: vscode.ExtensionContext): void {
  const channel = vscode.window.createOutputChannel('Drydock', { log: true })
  context.subscriptions.push(channel)
  const log: Log = redacting({ info: (l) => channel.info(l), warn: (l) => channel.warn(l) })

  const secrets: Secrets = {
    get: () => Promise.resolve(context.secrets.get(SECRET_KEY)),
    store: (v) => Promise.resolve(context.secrets.store(SECRET_KEY, v)),
    delete: () => Promise.resolve(context.secrets.delete(SECRET_KEY)),
  }
  const prompter: Prompter = {
    message: (level, text, actions = []) => {
      const show = level === 'error' ? vscode.window.showErrorMessage
        : level === 'warning' ? vscode.window.showWarningMessage : vscode.window.showInformationMessage
      return Promise.resolve(show(text, ...actions))
    },
    input: (opts) => Promise.resolve(vscode.window.showInputBox({
      prompt: opts.prompt, password: opts.password ?? false, ignoreFocusOut: opts.ignoreFocusOut ?? false,
      placeHolder: opts.placeHolder,
    })),
  }
  const window: Window = { remoteName: () => vscode.env.remoteName, focused: () => vscode.window.state.focused }
  const opener: Opener = {
    openExternal: async (url) => {
      await vscode.env.openExternal(vscode.Uri.parse(url, true))
    },
  }
  // `application` scope (package.json): only the laptop's user settings can
  // set it, never a workspace, folder or remote (§6.4).
  const settings: Settings = { url: () => vscode.workspace.getConfiguration('drydock').get<string>('url', '') }

  const credentials = new CredentialStore(secrets)
  const client = new DrydockClient({ transport: nodeTransport, credentials, origin: () => originOf(settings.url()) })
  const session: SessionDeps = { prompter, opener, settings, credentials, client, log }

  const handlers: Record<CommandId, Handler> = {
    'drydock.signIn': async () => {
      await signIn(session)
    },
    'drydock.signOut': () => signOut(session),
    'drydock.openInBrowser': () => openInBrowser(session),
    'drydock.showLog': async () => {
      channel.show(true)
    },
  }
  const dispatcher: Dispatcher = new Dispatcher(
    { prompter, window, opener, settings, log, signIn: async () => void (await dispatcher.run('drydock.signIn')()) },
    handlers,
  )
  for (const c of COMMANDS) {
    context.subscriptions.push(vscode.commands.registerCommand(c.id, dispatcher.run(c.id)))
  }

  // A token whose origin drydock.url no longer names: the operator is
  // asked, once the setting holds still (session.ts StaleTokenWatch), and
  // once now, for a change made while the extension was not running.
  const clock = {
    now: () => Date.now(),
    after: (ms: number, fn: () => void) => {
      const t = setTimeout(fn, ms)
      return () => clearTimeout(t)
    },
  }
  const stale = new StaleTokenWatch({ ...session, window, clock })
  context.subscriptions.push(
    stale,
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('drydock.url')) stale.changed()
    }),
    vscode.window.onDidChangeWindowState((s) => stale.focusChanged(s.focused)),
  )
  void stale.check().catch((e: unknown) => log.warn(`stale token: ${String(e)}`))
}

export function deactivate(): void {}
