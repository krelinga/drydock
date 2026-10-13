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

const parts = (v: string) => v.slice(1).split('.').map(Number)

/** -1, 0 or 1 as release `a` is older than, the same as, or newer than `b`. */
export function compareReleases(a: string, b: string): number {
  const x = parts(a)
  const y = parts(b)
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i]! < y[i]! ? -1 : 1
  return 0
}

/**
 * The quiet notice for a server whose release is not this extension's, or
 * null: only when both are release versions and they differ (a `dev` server
 * or extension says nothing; its version builds no URL), and not for a
 * version this VS Code was already told about and let be (`dismissed`, kept
 * per laptop, so a new window does not ask again). An older server's release
 * is said to be older: installing it goes back.
 */
export function versionNotice(meta: Meta | null, own = OWN_VERSION, dismissed: string | undefined = undefined): string | null {
  if (meta === null || vsixURL(meta.version) === null || vsixURL(own) === null || meta.version === own) return null
  if (dismissed === meta.version) return null
  return compareReleases(meta.version, own) < 0
    ? `This Drydock is ${meta.version}, older than this extension (${own}). Installing its extension goes back to ${meta.version}.`
    : `This Drydock is ${meta.version}, and this extension is ${own}.`
}

/**
 * What came of an offer: installed; let be by the operator (`dismissed`, or
 * no install was offered); or asked for where rule 1 refuses it (`refused`),
 * which is this window's limit, not the operator's answer.
 */
export type Offer = 'installed' | 'dismissed' | 'refused'

/** Offers, and on the operator's word installs, the extension matching `version`. */
export async function installMatching(d: UpdateDeps, version: string, sentence: string, level: 'info' | 'warning' = 'warning'): Promise<Offer> {
  if (vsixURL(version) === null) {
    await d.prompter.message(level, sentence)
    return 'dismissed'
  }
  const label = installLabel(version)
  if ((await d.prompter.message(level, sentence, [label])) !== label) return 'dismissed'
  if (!mayMutate(d.window.remoteName())) {
    await d.prompter.message('warning', ELSEWHERE)
    return 'refused'
  }
  d.log.info(`installing the extension for Drydock ${version}`)
  await d.updater.install(version)
  if ((await d.prompter.message('info', `Installed the extension for Drydock ${version}. Reload to use it.`, [RELOAD])) === RELOAD) {
    await d.updater.reload()
  }
  return 'installed'
}

/** Where the version the operator let be is kept: per laptop (the adapter's globalState). */
export interface Dismissals {
  get(): string | undefined
  set(version: string): Promise<void>
}

/**
 * §9.1's quiet notice for `meta` (null when the handshake's skew is not
 * `ok`), offered at most once per call. Only the operator's own let-be is
 * remembered: a container window refusing the install (rule 1) is not a
 * dismissal, so a local window still offers it.
 */
export async function offerVersion(d: UpdateDeps, meta: Meta | null, dismissals: Dismissals, own = OWN_VERSION): Promise<Offer | null> {
  const text = versionNotice(meta, own, dismissals.get())
  if (text === null || meta === null) return null
  const offer = await installMatching(d, meta.version, text, 'info')
  if (offer === 'dismissed') await dismissals.set(meta.version)
  return offer
}
