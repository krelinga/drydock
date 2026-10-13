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
// told, with the choice to forget it anyway.

import { ApiError } from '../../web/src/core/errors'
import { describeError } from '../../web/src/core/messages'
import type { DrydockClient } from './client'
import { checkPaste, type CredentialStore } from './credential'
import { parseMeta, skew, type Meta } from './handshake'
import type { Log, Opener, Prompter, Settings } from './ports'
import { pathFor } from './routes'
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
      d.log.warn(`sign in: ${e instanceof ApiError ? `${e.status} ${e.code}` : String(e)}`)
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
  const o = originOf(d.settings.url())
  if (!o.ok || o.origin !== stored.origin) {
    // The token is for a Drydock this window is no longer configured for:
    // the client will not send it there, or anywhere else.
    const ans = await d.prompter.message('warning',
      `The stored token is for ${stored.origin}, which drydock.url no longer names, so it cannot be revoked from here. Revoke it in that Drydock's Settings.`,
      [FORGET_ANYWAY])
    if (ans === FORGET_ANYWAY) await d.credentials.clear()
    return
  }
  try {
    await d.client.send('DELETE', pathFor('auth.token.delete'))
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      // Already dead; the client cleared it.
      await d.prompter.message('info', 'Signed out of Drydock.')
      return
    }
    const ans = await d.prompter.message('error',
      `${describeError(e)} The token was not revoked and is still stored. Try again, or revoke it in Drydock's Settings.`,
      [FORGET_ANYWAY])
    if (ans === FORGET_ANYWAY) await d.credentials.clear()
    return
  }
  await d.credentials.clear()
  d.log.info(`signed out of ${stored.origin}; the token revoked itself`)
  await d.prompter.message('info', 'Signed out of Drydock. The token is revoked.')
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
