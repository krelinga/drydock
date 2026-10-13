// Keeping the extension in step with the Drydock it talks to (vscode-
// extension §9.1, §9.2). VS Code does not update a sideloaded extension, so
// the handshake does: a server whose release differs from this extension's
// gets one quiet notice per window, and a server outside the api range
// replaces every mutation with *Install the matching extension*. Either way
// the install is the server's own release's .vsix, downloaded from GitHub
// and checked against that release's SHA256SUMS (node/update.ts), then
// handed to VS Code. Installing an extension is a mutation of the laptop,
// so it is held to rule 1 like any other (§8).

import pkg from '../package.json'
import { vsixURL, type Meta } from './handshake'
import type { Log, Prompter, Window } from './ports'
import { ELSEWHERE, mayMutate } from './windowRule'

export const OWN_VERSION = `v${pkg.version}`

/** Installs release `version`'s checked .vsix (the adapter: download, verify, installExtension). */
export interface Updater {
  install(version: string): Promise<void>
  /** Reloads the window, so the installed version runs. */
  reload(): Promise<void>
}

export interface UpdateDeps {
  prompter: Prompter
  window: Window
  log: Log
  updater: Updater
}

export const installLabel = (version: string) => `Install the extension for Drydock ${version}`
export const RELOAD = 'Reload Window'

/**
 * The quiet notice for a server whose release is not this extension's, or
 * null: only when both are release versions and they differ (a `dev` server
 * or extension says nothing; its version builds no URL).
 */
export function versionNotice(meta: Meta | null, own = OWN_VERSION): string | null {
  if (meta === null || vsixURL(meta.version) === null || vsixURL(own) === null || meta.version === own) return null
  return `This Drydock is ${meta.version}, and this extension is ${own}.`
}

/** Offers, and on the operator's word installs, the extension matching `version`. Resolves to whether it installed. */
export async function installMatching(d: UpdateDeps, version: string, sentence: string, level: 'info' | 'warning' = 'warning'): Promise<boolean> {
  if (vsixURL(version) === null) {
    await d.prompter.message(level, sentence)
    return false
  }
  const label = installLabel(version)
  if ((await d.prompter.message(level, sentence, [label])) !== label) return false
  if (!mayMutate(d.window.remoteName())) {
    await d.prompter.message('warning', ELSEWHERE)
    return false
  }
  d.log.info(`installing the extension for Drydock ${version}`)
  await d.updater.install(version)
  if ((await d.prompter.message('info', `Installed the extension for Drydock ${version}. Reload to use it.`, [RELOAD])) === RELOAD) {
    await d.updater.reload()
  }
  return true
}
