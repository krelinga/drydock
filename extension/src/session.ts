// Signing in and out (vscode-extension §4.2).
//
// Sign In: the token is minted in Drydock's Settings (Create a token in
// Drydock opens it), pasted into a password box, shape-checked, and verified
// with GET /api/meta before anything is stored. Only on a 200 is
// `{origin, token}` written to SecretStorage. The extension never sees the
// password and never calls the sign-in route.
//
// Sign Out: the token revokes itself (DELETE /api/auth/token), then the
// entry is cleared, so signing out ends the credential rather than leaving it
// live and forgotten. A token the server already refuses (401) is cleared
// too; when Drydock cannot be reached, the token is kept and the operator is
// told, with the choice to forget it anyway. The token is revoked at the
// origin it was stored for, whatever drydock.url says now.
//
// No live token is left behind when the credential is replaced: a sign-in
// with a new token revokes the old one at its own origin, best effort, and
// then forgets it; a revocation that fails is said, with where to revoke it
// by hand. A drydock.url that no longer names the stored token's origin
// revokes nothing by itself: StaleTokenWatch asks.

import { ApiError } from '../../web/src/core/errors'
import { describeError } from '../../web/src/core/messages'
import type { DrydockClient } from './client'
import { checkPaste, type Credential, type CredentialStore } from './credential'
import { parseMeta, skew, type Meta } from './handshake'
import { errorLine } from './log'
import type { Clock, Log, Opener, Prompter, Settings, Window } from './ports'
import { originOf } from './settings'
import { mayMutate } from './windowRule'

export const CREATE_TOKEN = 'Create a token in Drydock'
export const PASTE_TOKEN = 'Paste a token'
export const SIGN_IN_INTRO =
  "Sign in with an API token from Drydock's Settings. Tick approve only if this VS Code should approve host access."
export const TOKEN_REFUSED =
  'Drydock refused this token: it was revoked or has expired, or this Drydock is older than the extension and accepts no tokens (then upgrade Drydock).'
export const FORGET_ANYWAY = 'Forget it anyway'

export interface SessionDeps {
  prompter: Prompter
  opener: Opener
  settings: Settings
  credentials: CredentialStore
  client: DrydockClient
  log: Log
  /**
   * Applies a change of the stored credential to the window (the adapter's
   * CredentialWatch), called right after each write or clear here and before
   * anything is shown: a notification's promise settles only when it is
   * closed, and a command the operator runs meanwhile must find the stream
   * already on the new credential.
   */
  applied?: () => Promise<unknown>
}

/** drydock.signIn. Resolves to the verified meta, or null when nothing was stored. */
export async function signIn(d: SessionDeps): Promise<Meta | null> {
  const o = originOf(d.settings.url())
  if (!o.ok) {
    await d.prompter.message('warning', o.sentence)
    return null
  }
  const choice = await d.prompter.message('info', SIGN_IN_INTRO, [PASTE_TOKEN, CREATE_TOKEN])
  if (choice === undefined) return null
  if (choice === CREATE_TOKEN) await d.opener.openExternal(`${o.origin}/settings#tokens`)

  const raw = await d.prompter.input({
    prompt: `Paste the API token for ${o.origin}`,
    password: true,
    ignoreFocusOut: true,
  })
  if (raw === undefined) return null
  const pasted = checkPaste(raw)
  if (!pasted.ok) {
    await d.prompter.message('error', pasted.sentence)
    return null
  }

  let body: unknown
  try {
    body = await d.client.metaWith(o.origin, pasted.token)
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      await d.prompter.message('error', TOKEN_REFUSED)
    } else {
      d.log.warn(`sign in: ${errorLine(e)}`)
      await d.prompter.message('error', describeError(e))
    }
    return null
  }
  const meta = parseMeta(body)
  if (meta === null) {
    await d.prompter.message('error', 'Drydock answered in a way this extension cannot read. Show Log has the details.')
    d.log.warn('sign in: GET /api/meta answered with a body that is not a meta answer')
    return null
  }
  const old = await d.credentials.read()
  if (old !== null && (old.origin !== o.origin || old.token !== pasted.token)) await retire(d, old)
  await d.credentials.write({ origin: o.origin, token: pasted.token })
  await apply(d)
  d.log.info(`signed in to ${o.origin} (Drydock ${meta.version}, api ${meta.api}) as "${meta.credential.label}", scopes ${meta.credential.scopes.join(', ')}`)
  const s = skew(meta.api)
  const signed = `Signed in to Drydock ${meta.version} as "${meta.credential.label}".`
  await d.prompter.message(s.kind === 'ok' ? 'info' : 'warning', s.kind === 'ok' ? signed : `${signed} ${s.sentence}`)
  return meta
}

/** drydock.signOut. */
export async function signOut(d: SessionDeps): Promise<void> {
  const stored = await d.credentials.read()
  if (stored === null) {
    await d.prompter.message('info', 'Not signed in to Drydock.')
    return
  }
  try {
    await d.client.revokeAt(stored.origin, stored.token)
  } catch (e) {
    if (!(e instanceof ApiError && e.status === 401)) {
      d.log.warn(`sign out: ${errorLine(e)}`)
      const ans = await d.prompter.message('error',
        `${describeError(e)} The token was not revoked and is still stored. Try again, or revoke it in Drydock's Settings at ${stored.origin}.`,
        [FORGET_ANYWAY])
      if (ans === FORGET_ANYWAY) {
        await d.credentials.clear()
        await apply(d)
      }
      return
    }
    // A 401: the token was already revoked or expired.
  }
  await d.credentials.clear()
  await apply(d)
  d.log.info(`signed out of ${stored.origin}; the token is revoked`)
  await d.prompter.message('info', 'Signed out of Drydock. The token is revoked.')
}

/**
 * Revokes `old` at its own origin, best effort, and forgets it whatever the
 * answer: the caller is replacing it. A failure is logged and said.
 */
async function retire(d: SessionDeps, old: Credential): Promise<void> {
  try {
    await d.client.revokeAt(old.origin, old.token)
    d.log.info(`revoked the previous token at ${old.origin}`)
  } catch (e) {
    if (!(e instanceof ApiError && e.status === 401)) {
      d.log.warn(`revoking the previous token at ${old.origin}: ${errorLine(e)}`)
      void d.prompter.message('warning', `${notRevoked(old.origin)}`)
    }
  }
  await d.credentials.clear()
}

/**
 * Applies a credential change to the window (`applied`). The change itself
 * is done by now, so a failure here is the window's refresh failing, never
 * the sign-in or sign-out: it is logged, and the command goes on to say what
 * it did.
 */
async function apply(d: SessionDeps): Promise<void> {
  try {
    await d.applied?.()
  } catch (e) {
    d.log.warn(`refreshing this window after the credential changed: ${errorLine(e)}`)
  }
}

export const notRevoked = (origin: string) =>
  `The previous Drydock token could not be revoked at ${origin}, and is forgotten here. Revoke it in that Drydock's Settings.`

export const KEEP = 'Keep'
export const signOutOf = (origin: string) => `Sign out of ${origin}`
export const staleSentence = (stored: string, now: string) =>
  `drydock.url now names ${now}, but this VS Code is signed in to ${stored}. That token is not sent anywhere while drydock.url names another Drydock.`

/** How long drydock.url must hold still before a change is acted on: the Settings editor writes on every keystroke. */
export const SETTLE_MS = 3_000

export interface StaleDeps extends SessionDeps {
  window: Window
  clock: Clock
}

/**
 * A stored token whose origin drydock.url no longer names (vscode-extension
 * §6.4). Nothing is revoked automatically: the client already sends the
 * token nowhere while the setting names another origin (client.ts), and a
 * changed setting may be a half-typed one, or a switch the operator means to
 * undo. So, once the setting has held still for SETTLE_MS and names a valid
 * origin other than the token's, the operator is asked once — Sign out of
 * the old Drydock (revoke there, best effort, and forget) or Keep (nothing
 * sent) — and only:
 *
 *   - from a local or SSH window (§8 rule 1: signing out is a mutation);
 *   - in the focused window, so one change is one question however many
 *     windows are open — and when a window gains the focus it checks again
 *     (`focusChanged`), so a question no focused window was there to ask is
 *     not stranded;
 *   - once per (token origin, new origin) pair in this window, and never
 *     while a question is already open.
 *
 * An empty or invalid value does nothing: it names no other Drydock.
 */
export class StaleTokenWatch {
  #cancel: (() => void) | null = null
  #asking = false
  readonly #asked = new Set<string>()

  constructor(private readonly d: StaleDeps) {}

  /** drydock.url changed: act once it has held still for SETTLE_MS. */
  changed(): void {
    this.#cancel?.()
    this.#cancel = this.d.clock.after(SETTLE_MS, () => {
      this.#cancel = null
      void this.check().catch((e: unknown) => this.d.log.warn(`stale token: ${errorLine(e)}`))
    })
  }

  /** The window's focus changed: a window that gains it checks, since a settle it missed asked nothing. */
  focusChanged(focused: boolean): void {
    // A settle still pending will check when it fires.
    if (!focused || this.#cancel !== null) return
    void this.check().catch((e: unknown) => this.d.log.warn(`stale token: ${errorLine(e)}`))
  }

  /** Cancels a pending settle; the adapter registers it to run on deactivate. */
  dispose(): void {
    this.#cancel?.()
    this.#cancel = null
  }

  /** Asks now, if there is anything to ask. Resolves to what was done. */
  async check(): Promise<'nothing' | 'kept' | 'signed_out'> {
    const d = this.d
    if (!mayMutate(d.window.remoteName()) || !d.window.focused() || this.#asking) return 'nothing'
    const o = originOf(d.settings.url())
    if (!o.ok) return 'nothing'
    const stored = await d.credentials.read()
    if (stored === null || stored.origin === o.origin) return 'nothing'
    const pair = `${stored.origin} ${o.origin}`
    if (this.#asked.has(pair)) return 'nothing'
    this.#asked.add(pair)
    this.#asking = true
    let ans: string | undefined
    try {
      ans = await d.prompter.message('info', staleSentence(stored.origin, o.origin), [signOutOf(stored.origin), KEEP])
    } finally {
      this.#asking = false
    }
    if (ans !== signOutOf(stored.origin)) return 'kept'
    // The entry may have changed while the question was open: retire only
    // the token that was asked about.
    const now = await d.credentials.read()
    if (now === null || now.origin !== stored.origin || now.token !== stored.token) return 'nothing'
    await retire(d, stored)
    await apply(d)
    await d.prompter.message('info', `Signed out of ${stored.origin}. Sign in to ${o.origin} with a token from its Settings.`)
    return 'signed_out'
  }
}

/** drydock.openInBrowser: Drydock's home page, in the browser. */
export async function openInBrowser(d: Pick<SessionDeps, 'prompter' | 'opener' | 'settings'>): Promise<void> {
  const o = originOf(d.settings.url())
  if (!o.ok) {
    await d.prompter.message('warning', o.sentence)
    return
  }
  await d.opener.openExternal(`${o.origin}/`)
}
