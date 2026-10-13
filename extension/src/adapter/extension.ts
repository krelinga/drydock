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
import type { Clock, Extensions, Log, Opener, Prompter, Secrets, Settings, Window } from '../ports'
import { Live } from '../live'
import { cloneCommand, connectCommand, go, openInDrydock, refreshRepositories, type FlowDeps } from '../flows'
import { statusView } from '../status'
import { offerVersion, type Updater } from '../update'
import { downloadRelease, removeDownload } from '../node/update'
import { Reviews, SCHEME, approve, decline, idOfURI, reviewApproval, type DocumentsPort, type ReviewDeps } from '../review'
import { repositoryNodes, workspaceNodes, type TreeNode } from '../tree'
import { StaleTokenWatch, openInBrowser, signIn, signOut, type SessionDeps } from '../session'
import { originOf } from '../settings'
import { nodeTransport } from '../node/transport'

/** The one SecretStorage key: `{origin, token}` (credential.ts). */
const SECRET_KEY = 'drydock.credential'

/**
 * The extension-host tier's seam (vscode-extension §12.5): in
 * ExtensionMode.Test only, `activate` returns this, and a spec sets a
 * scripted Prompter, a recording Opener, a Window reporting any remoteName
 * and fake Extensions in place of VS Code's. In Production `activate`
 * returns undefined, so the seam cannot ship live (the Production run
 * asserts `exports === undefined`).
 */
export interface Seam {
  prompter?: Prompter
  opener?: Opener
  window?: Window
  extensions?: Extensions
}

export function activate(context: vscode.ExtensionContext): Seam | undefined {
  const seam: Seam = {}
  const channel = vscode.window.createOutputChannel('Drydock', { log: true })
  context.subscriptions.push(channel)
  const log: Log = redacting({ info: (l) => channel.info(l), warn: (l) => channel.warn(l) })

  const secrets: Secrets = {
    get: () => Promise.resolve(context.secrets.get(SECRET_KEY)),
    store: (v) => Promise.resolve(context.secrets.store(SECRET_KEY, v)),
    delete: () => Promise.resolve(context.secrets.delete(SECRET_KEY)),
  }
  const realPrompter: Prompter = {
    message: (level, text, actions = []) => {
      const show = level === 'error' ? vscode.window.showErrorMessage
        : level === 'warning' ? vscode.window.showWarningMessage : vscode.window.showInformationMessage
      return Promise.resolve(show(text, ...actions))
    },
    input: (opts) => {
      const cts = new vscode.CancellationTokenSource()
      opts.cancel?.onCancel(() => cts.cancel())
      return Promise.resolve(vscode.window.showInputBox({
        prompt: opts.prompt, password: opts.password ?? false, ignoreFocusOut: opts.ignoreFocusOut ?? false,
        placeHolder: opts.placeHolder,
      }, cts.token)).finally(() => cts.dispose())
    },
    confirm: async (text, action) => (await vscode.window.showWarningMessage(text, { modal: true }, action)) === action,
    pick: async (items, opts) => {
      const picked = await vscode.window.showQuickPick(items.map((i) => ({ ...i, picked: false })), {
        placeHolder: opts.placeHolder, matchOnDescription: true,
      })
      return picked?.value
    },
    progress: (title, run) => Promise.resolve(vscode.window.withProgress(
      { location: vscode.ProgressLocation.Notification, title, cancellable: true },
      (p, token) => run((message) => p.report({ message }), () => token.isCancellationRequested),
    )),
  }
  const realWindow: Window = { remoteName: () => vscode.env.remoteName, focused: () => vscode.window.state.focused }
  const realOpener: Opener = {
    openExternal: async (url) => {
      await vscode.env.openExternal(vscode.Uri.parse(url, true))
    },
    // §7.1: the remote folder, in a new window; never the vscode:// link.
    openFolder: async (uri) => {
      await vscode.commands.executeCommand('vscode.openFolder', vscode.Uri.parse(uri, true), { forceNewWindow: true })
    },
  }
  const realExtensions: Extensions = {
    installed: (id) => vscode.extensions.getExtension(id) !== undefined,
    install: async (id) => {
      await vscode.commands.executeCommand('workbench.extensions.installExtension', id)
    },
  }
  // Each port is VS Code's unless the Test-mode seam holds a replacement.
  const prompter: Prompter = {
    message: (l, t, a) => (seam.prompter ?? realPrompter).message(l, t, a),
    input: (o) => (seam.prompter ?? realPrompter).input(o),
    pick: (i, o) => (seam.prompter ?? realPrompter).pick(i, o),
    progress: (t, r) => (seam.prompter ?? realPrompter).progress(t, r),
    confirm: (t, a) => (seam.prompter ?? realPrompter).confirm(t, a),
  }
  const window: Window = {
    remoteName: () => (seam.window ?? realWindow).remoteName(),
    focused: () => (seam.window ?? realWindow).focused(),
  }
  const opener: Opener = {
    openExternal: (u) => (seam.opener ?? realOpener).openExternal(u),
    openFolder: (u) => (seam.opener ?? realOpener).openFolder(u),
  }
  const extensions: Extensions = {
    installed: (id) => (seam.extensions ?? realExtensions).installed(id),
    install: (id) => (seam.extensions ?? realExtensions).install(id),
  }
  // `application` scope (package.json): only the laptop's user settings can
  // set it, never a workspace, folder or remote (§6.4).
  const settings: Settings = { url: () => vscode.workspace.getConfiguration('drydock').get<string>('url', '') }

  const clock: Clock = {
    now: () => Date.now(),
    after: (ms: number, fn: () => void) => {
      const t = setTimeout(fn, ms)
      return () => clearTimeout(t)
    },
  }
  const credentials = new CredentialStore(secrets)
  const client = new DrydockClient({ transport: nodeTransport, credentials, origin: () => originOf(settings.url()) })
  const session: SessionDeps = { prompter, opener, settings, credentials, client, log }

  // The window's live state: the reducer over the stream (live.ts), and the
  // flows over it (flows.ts).
  const live = new Live({ client, clock, log })
  context.subscriptions.push({ dispose: () => live.close() })
  // §9.2's update path: the release's .vsix, checked, then installed by VS Code.
  const updater: Updater = {
    install: async (version) => {
      const file = await downloadRelease(version)
      try {
        await vscode.commands.executeCommand('workbench.extensions.installExtension', vscode.Uri.file(file))
      } finally {
        // VS Code has copied it, or failed to: either way the download goes.
        removeDownload(file)
      }
    },
    reload: async () => {
      await vscode.commands.executeCommand('workbench.action.reloadWindow')
    },
  }
  const flows: FlowDeps = {
    live, client, prompter, opener, extensions, window, settings, clock, log, ready: new Set(), updater,
    connectWhenReady: () => vscode.workspace.getConfiguration('drydock').get<boolean>('connectWhenReady', false),
  }

  // The status bar (status.ts), rendered on every change and every few
  // seconds, for the quiet marker.
  const bar = vscode.window.createStatusBarItem('drydock.status', vscode.StatusBarAlignment.Left, 0)
  bar.name = 'Drydock'
  context.subscriptions.push(bar)
  let signedIn = false
  const render = () => {
    const v = statusView(live, clock.now(), signedIn)
    bar.text = v.text
    bar.tooltip = v.tooltip
    bar.command = v.command
    bar.backgroundColor = v.warning ? new vscode.ThemeColor('statusBarItem.warningBackground') : undefined
    bar.show()
  }
  const reread = async () => {
    const c = await credentials.read()
    const was = signedIn
    signedIn = c !== null
    // The views' welcome content (package.json viewsWelcome) reads this.
    await vscode.commands.executeCommand('setContext', 'drydock.signedIn', signedIn)
    // A stream belongs to the credential it opened with: signed out, or
    // signed in with another token (the old one is revoked), it closes, and
    // the next use opens one with the token now stored.
    if (!signedIn) live.close('signed_out')
    // The secret changed while signed in before and after: a new token.
    else if (was) live.close('signed_in_again')
    render()
  }
  context.subscriptions.push(
    { dispose: live.onChange(render) },
    context.secrets.onDidChange((e) => {
      if (e.key === SECRET_KEY) void reread()
    }),
  )
  // The two tree views (tree.ts). A shown view holds the stream (§5.5).
  const trees = [
    { view: 'drydock.workspaces', nodes: () => workspaceNodes(live.entities) },
    { view: 'drydock.repositories', nodes: () => repositoryNodes(live.entities) },
  ].map(({ view, nodes }) => {
    const provider = new NodeTree(nodes)
    const tv = vscode.window.createTreeView(view, { treeDataProvider: provider })
    // One hold while the view is visible. A hold still being taken when the
    // view is hidden is released as soon as it arrives, never kept.
    let release: (() => void) | null = null
    let visible = false
    let taking = false
    const follow = (shown: boolean) => {
      visible = shown
      if (shown && release === null && !taking) {
        taking = true
        live.hold().then((r) => {
          taking = false
          if (visible) release = r
          else r()
        }, (err: unknown) => {
          taking = false
          log.warn(`${view}: ${String(err)}`)
        })
      } else if (!shown && release !== null) {
        release()
        release = null
      }
    }
    context.subscriptions.push(tv, tv.onDidChangeVisibility((e) => follow(e.visible)), { dispose: () => follow(false) })
    // A view already shown when the extension activates holds the stream now.
    if (tv.visible) follow(true)
    return provider
  })
  context.subscriptions.push({ dispose: live.onChange(() => { for (const t of trees) t.refresh() }) })

  // One quiet notice per window when this Drydock's release is not this
  // extension's, once the handshake says so (§9.1).
  // A version let be is remembered per laptop (globalState), so a new
  // window does not ask again about the same release.
  const DISMISSED = 'drydock.versionNotice.dismissed'
  let noticed = false
  context.subscriptions.push({ dispose: live.onChange(() => {
    if (noticed || live.meta === null) return
    noticed = true
    const dismissals = {
      get: () => context.globalState.get<string>(DISMISSED),
      set: async (v: string) => { await context.globalState.update(DISMISSED, v) },
    }
    const meta = live.skew()?.kind === 'ok' ? live.meta : null
    offerVersion({ prompter, window, log, updater }, meta, dismissals).catch((e: unknown) => log.warn(`update: ${String(e)}`))
  }) })

  const tick = setInterval(render, 2_000)
  context.subscriptions.push({ dispose: () => clearInterval(tick) })
  void reread()

  // The host-access review documents (review.ts): drydock-approval:/<id>,
  // read-only, rendered from the snapshot the approval sends the hash of.
  const docsChanged = new vscode.EventEmitter<vscode.Uri>()
  const documents: DocumentsPort = {
    show: async (uri) => {
      const doc = await vscode.workspace.openTextDocument(vscode.Uri.parse(uri, true))
      await vscode.window.showTextDocument(doc, { viewColumn: vscode.ViewColumn.Beside, preview: true })
    },
    activeUri: () => vscode.window.activeTextEditor?.document.uri.toString(),
    changed: (uri) => docsChanged.fire(vscode.Uri.parse(uri, true)),
  }
  const reviews = new Reviews({ ...flows, documents })
  const reviewDeps: ReviewDeps = { ...flows, documents, reviews }
  reviewDeps.review = flows.review = (id) => reviewApproval(reviewDeps, id)
  context.subscriptions.push(docsChanged, vscode.workspace.registerTextDocumentContentProvider(SCHEME, {
    onDidChange: docsChanged.event,
    provideTextDocumentContent: (uri) => {
      const id = idOfURI(uri.toString())
      return id === null ? '' : reviews.text(id)
    },
  }))

  const handlers: Record<CommandId, Handler> = {
    'drydock.signIn': async () => {
      await signIn(session)
    },
    'drydock.signOut': () => signOut(session),
    'drydock.openInBrowser': (id) => (id === undefined ? openInBrowser(session) : openInDrydock(flows, id)),
    'drydock.showLog': async () => {
      channel.show(true)
    },
    'drydock.go': () => go(flows),
    'drydock.clone': () => cloneCommand(flows),
    'drydock.connect': (id) => connectCommand(flows, id),
    'drydock.refreshRepositories': () => refreshRepositories(flows),
    'drydock.reviewApproval': (id) => reviewApproval(reviewDeps, id),
    'drydock.approve': () => approve(reviewDeps),
    'drydock.decline': () => decline(reviewDeps),
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
  const stale = new StaleTokenWatch({ ...session, window, clock })
  context.subscriptions.push(
    stale,
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('drydock.url')) {
        stale.changed()
        // The stream was for the old origin; the next use opens one for the new.
        live.close('url_changed')
        render()
      }
    }),
    vscode.window.onDidChangeWindowState((s) => stale.focusChanged(s.focused)),
  )
  void stale.check().catch((e: unknown) => log.warn(`stale token: ${String(e)}`))
  return context.extensionMode === vscode.ExtensionMode.Test ? seam : undefined
}

export function deactivate(): void {}

/** A TreeDataProvider over tree.ts's nodes, rebuilt from the entities on every change. */
class NodeTree implements vscode.TreeDataProvider<TreeNode> {
  readonly #changed = new vscode.EventEmitter<void>()
  readonly onDidChangeTreeData = this.#changed.event

  constructor(private readonly nodes: () => TreeNode[]) {}

  refresh(): void {
    this.#changed.fire()
  }

  getChildren(n?: TreeNode): TreeNode[] {
    return n === undefined ? this.nodes() : n.children ?? []
  }

  getTreeItem(n: TreeNode): vscode.TreeItem {
    const item = new vscode.TreeItem(n.label, n.children !== undefined && n.children.length > 0
      ? vscode.TreeItemCollapsibleState.Collapsed : vscode.TreeItemCollapsibleState.None)
    item.id = n.key
    item.description = n.description
    item.tooltip = n.tooltip
    item.contextValue = n.contextValue
    item.iconPath = new vscode.ThemeIcon(n.icon)
    if (n.command !== undefined) item.command = { command: n.command.id, title: '', arguments: [n.command.arg] }
    return item
  }
}
