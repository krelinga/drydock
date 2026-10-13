// §8's rules as the dispatcher holds them (vscode-extension §12.2): every
// mutating command, run in an attached-container window with an operator
// who answers yes to everything, sends nothing and says to do it elsewhere;
// the same commands under a local or SSH window send (the control). Every
// command ignores its arguments and returns undefined.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../web/src/core/errors'
import { DrydockClient } from './client'
import { COMMANDS, type CommandId } from './commands'
import { CredentialStore } from './credential'
import { Dispatcher, OPEN_IN_DRYDOCK, TOKEN_ENDED, type Handler } from './dispatch'
import { openInBrowser, PASTE_TOKEN, signIn, signOut, type SessionDeps } from './session'
import { originOf } from './settings'
import { ELSEWHERE } from './windowRule'
import {
  FakeTransport, FakeWindow, MemoryLog, MemorySecrets, ORIGIN, RecordingOpener, ScriptedPrompter, TOKEN, TOKEN2,
  errorResponse, jsonResponse, settingsOf,
} from '../test/fakes'

const META = { version: 'v0.12.1', api: 1, credential: { kind: 'token', label: 'VS Code', scopes: ['read', 'lifecycle'], expires_at: '2027-01-01T00:00:00Z' } }

/** The real handlers over fakes, with an operator who says yes to everything. */
function world(remoteName: string | undefined) {
  const transport = new FakeTransport()
  transport.route = (req) => (req.url.endsWith('/api/meta') ? jsonResponse(200, META) : { status: 204, headers: { get: () => null }, text: async () => '' })
  const secrets = new MemorySecrets()
  secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN })
  const prompter = new ScriptedPrompter()
  // Every message answered with its first action, every input with a token.
  prompter.message = async function (level, text, actions = []) {
    this.shown.push({ level, text, actions })
    return actions[0]
  }
  prompter.typed = Array(10).fill(TOKEN2)
  const window = new FakeWindow(remoteName)
  const opener = new RecordingOpener()
  const settings = settingsOf(ORIGIN)
  const log = new MemoryLog()
  const credentials = new CredentialStore(secrets)
  const client = new DrydockClient({ transport, credentials, origin: () => originOf(settings.url()) })
  const session: SessionDeps = { prompter, opener, settings, credentials, client, log }
  const handlers: Record<CommandId, Handler> = {
    'drydock.signIn': async () => void (await signIn(session)),
    'drydock.signOut': () => signOut(session),
    'drydock.openInBrowser': () => openInBrowser(session),
    'drydock.showLog': async () => {},
  }
  const d: Dispatcher = new Dispatcher({ prompter, window, opener, settings, log, signIn: async () => void (await d.run('drydock.signIn')()) }, handlers)
  return { d, transport, secrets, prompter, opener, log }
}

const MUTATING = COMMANDS.filter((c) => c.mutating).map((c) => c.id)

describe('rule 1: no mutation outside local and SSH windows', () => {
  it('the registry marks the commands that change anything as mutating', () => {
    // A named list, so marking one of these read-only fails here by name.
    expect(MUTATING).toEqual(['drydock.signIn', 'drydock.signOut'])
  })

  it('every mutating command in an attached-container window sends nothing, stores nothing, and says why', async () => {
    for (const id of MUTATING) {
      const w = world('attached-container')
      const before = w.secrets.value
      await w.d.run(id)()
      expect(w.transport.requests, id).toEqual([])
      expect(w.secrets.value, id).toBe(before)
      expect(w.prompter.texts(), id).toEqual([ELSEWHERE])
      expect(w.prompter.shown[0]!.actions).toEqual([OPEN_IN_DRYDOCK])
      // Its one action opens Drydock, and only that.
      expect(w.opener.opened).toEqual([`${ORIGIN}/`])
    }
  })

  it('the same commands under a local and an SSH window do send (control)', async () => {
    for (const name of [undefined, 'ssh-remote']) {
      for (const id of MUTATING) {
        const w = world(name)
        await w.d.run(id)()
        expect(w.transport.requests.length, `${id} in ${name}`).toBeGreaterThan(0)
      }
    }
  })

  it('a read-only command still runs in a container window', async () => {
    const w = world('attached-container')
    await w.d.run('drydock.openInBrowser')()
    expect(w.opener.opened).toEqual([`${ORIGIN}/`])
    expect(w.prompter.texts()).toEqual([])
  })
})

describe('rules 2 and 3: arguments ignored, nothing returned', () => {
  it('every command, called with crafted arguments, returns undefined', async () => {
    for (const c of COMMANDS) {
      const w = world(undefined)
      const got = await w.d.run(c.id)('01JA0000000000000000000010', { confirm: 'krelinga/alpha', hash: 'sha256:x', token: TOKEN2 })
      expect(got, c.id).toBeUndefined()
    }
  })
})

describe('failures are shown, never thrown', () => {
  it('Sign Out of a token the server already refuses (401) forgets it', async () => {
    const w = world(undefined)
    w.transport.route = (req) => (req.url.endsWith('/api/meta') ? jsonResponse(200, META) : errorResponse(401, 'unauthenticated'))
    w.prompter.message = async function (level, text, actions = []) {
      this.shown.push({ level, text, actions })
      return undefined
    }
    await w.d.run('drydock.signOut')()
    expect(w.secrets.value).toBeUndefined()
    expect(w.prompter.texts()).toContain('Signed out of Drydock. The token is revoked.')
  })

  it('a handler that throws something unexpected is reported, and the command still returns undefined', async () => {
    const w = world(undefined)
    w.transport.route = () => {
      throw new Error('boom')
    }
    w.prompter.message = async function (level, text, actions = []) {
      this.shown.push({ level, text, actions })
      return actions.includes(PASTE_TOKEN) ? PASTE_TOKEN : undefined
    }
    expect(await w.d.run('drydock.signIn')()).toBeUndefined()
    expect(w.prompter.shown.at(-1)!.level).toBe('error')
  })

  it("an ApiError 401 from any handler says the token ended and offers Sign In; another code gets core's sentence (control)", async () => {
    const prompter = new ScriptedPrompter()
    const log = new MemoryLog()
    const throwing = (e: unknown): Record<CommandId, Handler> => ({
      'drydock.signIn': async () => {}, 'drydock.signOut': async () => {}, 'drydock.showLog': async () => {},
      'drydock.openInBrowser': async () => { throw e },
    })
    const deps = { prompter, window: new FakeWindow(), opener: new RecordingOpener(), settings: settingsOf(ORIGIN), log, signIn: async () => {} }
    await new Dispatcher(deps, throwing(new ApiError(401, 'unauthenticated'))).run('drydock.openInBrowser')()
    await new Dispatcher(deps, throwing(new ApiError(409, 'in_progress'))).run('drydock.openInBrowser')()
    expect(prompter.shown[0]).toMatchObject({ text: TOKEN_ENDED, actions: ['Sign In'] })
    expect(prompter.shown[1]!.text).toBe('Already in progress.')
  })
})
