// How every command runs (vscode-extension §8.1). The adapter registers each
// command in COMMANDS as `dispatcher.run(id)`, and nothing else, so these
// hold for all of them:
//
//  1. A mutating command, in a window that is neither local nor SSH, sends
//     nothing and runs nothing: it says to do it elsewhere and offers Open in
//     Drydock (rule 1, the primary control).
//  2. Arguments are ignored (rule 2): a handler takes none, so a caller
//     cannot choose a target, a hash or an answer.
//  3. Every command returns undefined (rule 3): no caller can read anything
//     back.
//  4. A failure is shown as core's sentence for its code, never thrown to
//     the caller.

import { ApiError } from '../../web/src/core/errors'
import { describeError } from '../../web/src/core/messages'
import { NotConfigured, SignInNeeded } from './client'
import { COMMANDS, type CommandId } from './commands'
import type { Log, Opener, Prompter, Settings, Window } from './ports'
import { originOf } from './settings'
import { ELSEWHERE, mayMutate } from './windowRule'

export const OPEN_IN_DRYDOCK = 'Open in Drydock'
export const SIGN_IN = 'Sign In'
/** The one 401 path's sentence (§4.2): the client has already cleared the token. */
export const TOKEN_ENDED = 'Your Drydock token was revoked or has expired. Sign in again with a new one.'

export interface DispatchDeps {
  prompter: Prompter
  window: Window
  opener: Opener
  settings: Settings
  log: Log
  /** Runs Sign In, for the action a SignInNeeded offers. */
  signIn: () => Promise<void>
}

export type Handler = () => Promise<void>

export class Dispatcher {
  constructor(
    private readonly deps: DispatchDeps,
    private readonly handlers: Readonly<Record<CommandId, Handler>>,
  ) {}

  /** The command `id` as VS Code calls it: arguments ignored, nothing returned. */
  run(id: CommandId): (...args: unknown[]) => Promise<undefined> {
    const def = COMMANDS.find((c) => c.id === id)
    if (def === undefined) throw new Error(`unknown command ${id}`)
    return async () => {
      if (def.mutating && !mayMutate(this.deps.window.remoteName())) {
        this.deps.log.info(`${id}: refused in a ${this.deps.window.remoteName() ?? 'local'} window (§8 rule 1)`)
        await this.offerDrydock(ELSEWHERE)
        return undefined
      }
      try {
        await this.handlers[id]()
      } catch (e) {
        await this.report(id, e)
      }
      return undefined
    }
  }

  private async report(id: CommandId, e: unknown): Promise<void> {
    const p = this.deps.prompter
    if (e instanceof SignInNeeded) {
      if ((await p.message('warning', e.message, [SIGN_IN])) === SIGN_IN) await this.deps.signIn()
      return
    }
    if (e instanceof NotConfigured) {
      await p.message('warning', e.sentence)
      return
    }
    if (e instanceof ApiError && e.status === 401) {
      this.deps.log.warn(`${id}: 401, the stored token was cleared`)
      if ((await p.message('warning', TOKEN_ENDED, [SIGN_IN])) === SIGN_IN) await this.deps.signIn()
      return
    }
    if (e instanceof ApiError) {
      this.deps.log.warn(`${id}: ${e.status} ${e.code}`)
      await p.message('error', describeError(e))
      return
    }
    this.deps.log.warn(`${id}: ${e instanceof Error ? e.message : String(e)}`)
    await p.message('error', 'Something went wrong in the Drydock extension. Show Log has the details.')
  }

  /** `sentence`, with Open in Drydock when there is a Drydock to open. */
  async offerDrydock(sentence: string): Promise<void> {
    const o = originOf(this.deps.settings.url())
    if (!o.ok) {
      await this.deps.prompter.message('warning', sentence)
      return
    }
    if ((await this.deps.prompter.message('warning', sentence, [OPEN_IN_DRYDOCK])) === OPEN_IN_DRYDOCK) {
      await this.deps.opener.openExternal(o.origin + '/')
    }
  }
}
