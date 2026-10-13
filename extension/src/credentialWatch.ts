// What a change of the stored credential does to the window's stream
// (vscode-extension §4.2, §5.5). A stream belongs to the credential it
// opened with: signed out, or signed in with another token (the old one is
// revoked), it closes, and the next hold opens one with the token now stored.
//
// The change is learned two ways, and both come here: SecretStorage's
// onDidChange (another window signed in or out, or the client's 401 path
// cleared the entry), which VS Code delivers some time after the write; and
// this window's own Sign In and Sign Out, which `check` right after they
// write or clear (session.ts `applied`), before they show anything: the
// "Signed in" notification's promise settles only when it is closed, so the
// operator's next command can run while it is still open. Without that, a
// command run after a sign-in took its hold on the old stream, and the late
// close stopped the stream under it: a clone then waited for an event
// nothing would carry.
//
// So a check acts only on a credential other than the one it last acted on:
// the late notification of a change already applied finds nothing new and
// leaves the stream the next command opened with the new token alone. Checks
// run one at a time, in order, so an older read never lands after a newer one.

import type { CredentialStore } from './credential'
import type { Live } from './live'

export class CredentialWatch {
  /** The credential last acted on (`origin token`), null for none; undefined before the first check. */
  #known: string | null | undefined = undefined
  #chain: Promise<unknown> = Promise.resolve()

  constructor(private readonly d: { credentials: Pick<CredentialStore, 'read'>; live: Pick<Live, 'close'> }) {}

  /** Reads the stored credential and closes the stream if it changed. Resolves to whether one is stored. */
  check(): Promise<boolean> {
    const run = this.#chain.then(() => this.#check())
    this.#chain = run.catch(() => {})
    return run
  }

  async #check(): Promise<boolean> {
    const c = await this.d.credentials.read()
    const now = c === null ? null : `${c.origin} ${c.token}`
    const was = this.#known
    if (now === was) return now !== null
    this.#known = now
    if (now === null) this.d.live.close('signed_out')
    // Signed in before and after: a new token.
    else if (was !== null && was !== undefined) this.d.live.close('signed_in_again')
    return now !== null
  }
}
