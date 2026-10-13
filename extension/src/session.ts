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
// with a new token, and a drydock.url that no longer names the stored
// token's origin (`retireStale`, run when the setting changes and on
// activation), each revoke the old token at its own origin, best effort, and
// then forget it. A revocation that fails is said, with where to revoke it
// by hand.

import { ApiError } from '../../web/src/core/errors'
import { describeError } from '../../web/src/core/messages'
import type { DrydockClient } from './client'
import { checkPaste, type Credential, type CredentialStore } from './credential'
import { parseMeta, skew, type Meta } from './handshake'
import { errorLine } from './log'
import type { Log, Opener, Prompter, Settings } from './ports'
import { originOf } from './settings'

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
      if (ans === FORGET_ANYWAY) await d.credentials.clear()
      return
    }
    // A 401: the token was already revoked or expired.
  }
  await d.credentials.clear()
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

export const notRevoked = (origin: string) =>
  `The previous Drydock token could not be revoked at ${origin}, and is forgotten here. Revoke it in that Drydock's Settings.`

/**
 * After drydock.url changed (and on activation): a stored token for an
 * origin the setting no longer names is revoked there and forgotten. One for
 * the configured origin is left alone. Resolves to whether one was retired.
 */
export async function retireStale(d: SessionDeps): Promise<boolean> {
  const stored = await d.credentials.read()
  if (stored === null) return false
  const o = originOf(d.settings.url())
  if (o.ok && o.origin === stored.origin) return false
  await retire(d, stored)
  await d.prompter.message('info', `drydock.url no longer names ${stored.origin}, so this VS Code signed out of it.`)
  return true
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
