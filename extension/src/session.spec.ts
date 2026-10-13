// Sign In and Sign Out (session.ts; vscode-extension §4.2, §12.2's token
// rows): a paste is stored only after GET /api/meta answers 200, bound to
// the origin it was verified against; Sign Out revokes the token itself
// before forgetting it.

import { describe, expect, it } from 'vitest'
import { DrydockClient } from './client'
import { CredentialStore } from './credential'
import {
  CREATE_TOKEN, FORGET_ANYWAY, KEEP, PASTE_TOKEN, SETTLE_MS, StaleTokenWatch, TOKEN_REFUSED, notRevoked, signIn, signOut, signOutOf,
  staleSentence, type SessionDeps,
} from './session'
import { SERVER_NEWER } from './handshake'
import { originOf } from './settings'
import {
  FakeClock, FakeTransport, FakeWindow, MemoryLog, flush, MemorySecrets, ORIGIN, RecordingOpener, ScriptedPrompter, TOKEN, TOKEN2, errorResponse,
  jsonResponse, settingsOf,
} from '../test/fakes'

const META = { version: 'v0.12.1', api: 1, credential: { kind: 'token', label: 'VS Code on laptop', scopes: ['read', 'lifecycle'], expires_at: '2027-01-01T00:00:00Z' } }

function setup(url = ORIGIN) {
  const transport = new FakeTransport()
  transport.route = () => jsonResponse(200, META)
  const secrets = new MemorySecrets()
  const prompter = new ScriptedPrompter()
  const opener = new RecordingOpener()
  const log = new MemoryLog()
  const settings = settingsOf(url)
  const credentials = new CredentialStore(secrets)
  const client = new DrydockClient({ transport, credentials, origin: () => originOf(settings.url()) })
  const d: SessionDeps = { prompter, opener, settings, credentials, client, log }
  return { d, transport, secrets, prompter, opener, log, settings }
}

describe('Sign In', () => {
  it('a pasted token is verified with GET /api/meta and only then stored, as {origin, token}', async () => {
    const t = setup()
    t.prompter.answers = [PASTE_TOKEN]
    t.prompter.typed = [`  ${TOKEN}\n`]
    const meta = await signIn(t.d)
    expect(meta?.credential.label).toBe('VS Code on laptop')
    expect(t.transport.requests).toHaveLength(1)
    expect(t.transport.requests[0]).toMatchObject({ method: 'GET', url: `${ORIGIN}/api/meta` })
    expect(t.transport.requests[0]!.headers.Authorization).toBe(`Bearer ${TOKEN}`)
    expect(JSON.parse(t.secrets.value!)).toEqual({ origin: ORIGIN, token: TOKEN })
    // The paste box hides what is typed and survives a click elsewhere.
    expect(t.prompter.inputs[0]).toMatchObject({ password: true })
    // Never logged, though the label is.
    expect(t.log.lines.join('\n')).not.toContain(TOKEN)
    expect(t.log.lines.join('\n')).toContain('VS Code on laptop')
  })

  it('a paste of the wrong shape sends nothing and stores nothing', async () => {
    const t = setup()
    t.prompter.answers = [PASTE_TOKEN]
    t.prompter.typed = ['ghp_' + 'x'.repeat(36)]
    expect(await signIn(t.d)).toBeNull()
    expect(t.transport.requests).toEqual([])
    expect(t.secrets.writes).toBe(0)
  })

  it('a token the server refuses (401) is not stored, and the sentence says why it may be', async () => {
    const t = setup()
    t.transport.route = () => errorResponse(401, 'unauthenticated')
    t.prompter.answers = [PASTE_TOKEN]
    t.prompter.typed = [TOKEN]
    expect(await signIn(t.d)).toBeNull()
    expect(t.secrets.writes).toBe(0)
    expect(t.prompter.texts()).toContain(TOKEN_REFUSED)
  })

  it('an answer that is not a meta body is not stored', async () => {
    const t = setup()
    t.transport.route = () => jsonResponse(200, { hello: 1 })
    t.prompter.answers = [PASTE_TOKEN]
    t.prompter.typed = [TOKEN]
    expect(await signIn(t.d)).toBeNull()
    expect(t.secrets.writes).toBe(0)
  })

  it('Create a token opens Settings at #tokens on the configured origin, then asks for the paste', async () => {
    const t = setup('https://drydock.test:8443')
    t.prompter.answers = [CREATE_TOKEN]
    t.prompter.typed = [TOKEN]
    await signIn(t.d)
    expect(t.opener.opened).toEqual(['https://drydock.test:8443/settings#tokens'])
    expect(JSON.parse(t.secrets.value!).origin).toBe('https://drydock.test:8443')
  })

  it('Escape at either step, or no drydock.url, sends nothing', async () => {
    const dismissed = setup()
    dismissed.prompter.answers = [undefined]
    const escaped = setup()
    escaped.prompter.answers = [PASTE_TOKEN]
    escaped.prompter.typed = [undefined]
    const unset = setup('')
    for (const t of [dismissed, escaped, unset]) {
      expect(await signIn(t.d)).toBeNull()
      expect(t.transport.requests).toEqual([])
    }
    expect(unset.prompter.texts()[0]).toContain('drydock.url')
  })

  it('a newer server is signed in to, with the skew said', async () => {
    const t = setup()
    t.transport.route = () => jsonResponse(200, { ...META, api: 99 })
    t.prompter.answers = [PASTE_TOKEN]
    t.prompter.typed = [TOKEN]
    await signIn(t.d)
    expect(t.secrets.value).toContain(TOKEN)
    expect(t.prompter.shown.at(-1)).toMatchObject({ level: 'warning' })
    expect(t.prompter.shown.at(-1)!.text).toContain(SERVER_NEWER)
  })
})

describe('Sign Out', () => {
  it('the token revokes itself, then is forgotten', async () => {
    const t = setup()
    t.secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN })
    t.transport.route = () => ({ status: 204, headers: { get: () => null }, text: async () => '' })
    await signOut(t.d)
    expect(t.transport.requests).toHaveLength(1)
    expect(t.transport.requests[0]).toMatchObject({ method: 'DELETE', url: `${ORIGIN}/api/auth/token` })
    expect(t.transport.requests[0]!.headers.Origin).toBe(ORIGIN)
    expect(t.secrets.value).toBeUndefined()
  })

  it('when Drydock cannot be reached the token is kept, unless the operator forgets it anyway', async () => {
    const kept = setup()
    kept.secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN })
    kept.transport.route = () => errorResponse(503, 'unavailable')
    await signOut(kept.d)
    expect(kept.secrets.value).toContain(TOKEN)
    expect(kept.prompter.shown[0]!.actions).toEqual([FORGET_ANYWAY])

    const forgotten = setup()
    forgotten.secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN })
    forgotten.transport.route = () => errorResponse(503, 'unavailable')
    forgotten.prompter.answers = [FORGET_ANYWAY]
    await signOut(forgotten.d)
    expect(forgotten.secrets.value).toBeUndefined()
  })

  it('a token is revoked at the origin it was stored for, never at the one drydock.url names now', async () => {
    const t = setup('https://other.test')
    t.secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN2 })
    t.transport.route = () => ({ status: 204, headers: { get: () => null }, text: async () => '' })
    await signOut(t.d)
    expect(t.transport.requests.map((r) => [r.method, r.url, r.headers.Authorization])).toEqual(
      [['DELETE', `${ORIGIN}/api/auth/token`, `Bearer ${TOKEN2}`]])
    expect(t.secrets.value).toBeUndefined()
  })
})

describe('no live token left behind', () => {
  const ok204 = () => ({ status: 204, headers: { get: () => null }, text: async () => '' })

  it('signing in to another origin revokes the old token at its own origin, then stores the new one', async () => {
    const t = setup('https://new.test')
    t.secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN2 })
    t.transport.route = (req) => (req.method === 'DELETE' ? ok204() : jsonResponse(200, META))
    t.prompter.answers = [PASTE_TOKEN]
    t.prompter.typed = [TOKEN]
    await signIn(t.d)
    expect(t.transport.requests.map((r) => [r.method, r.url, r.headers.Authorization])).toEqual([
      ['GET', 'https://new.test/api/meta', `Bearer ${TOKEN}`],
      ['DELETE', `${ORIGIN}/api/auth/token`, `Bearer ${TOKEN2}`],
    ])
    expect(JSON.parse(t.secrets.value!)).toEqual({ origin: 'https://new.test', token: TOKEN })
  })

  it('signing in again with the same token revokes nothing (control)', async () => {
    const t = setup()
    t.secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN })
    t.prompter.answers = [PASTE_TOKEN]
    t.prompter.typed = [TOKEN]
    await signIn(t.d)
    expect(t.transport.requests.map((r) => r.method)).toEqual(['GET'])
  })

  it('a sign-in whose revocation of the old token fails still forgets it, and says where to revoke it by hand', async () => {
    const t = setup('https://new.test')
    t.secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN2 })
    t.transport.route = (req) => {
      if (req.method === 'DELETE') throw new Error('down')
      return jsonResponse(200, META)
    }
    t.prompter.answers = [PASTE_TOKEN]
    t.prompter.typed = [TOKEN]
    await signIn(t.d)
    expect(JSON.parse(t.secrets.value!)).toEqual({ origin: 'https://new.test', token: TOKEN })
    expect(t.prompter.texts()).toContain(notRevoked(ORIGIN))
  })
})

describe('a token for an origin drydock.url no longer names', () => {
  const ok204 = () => ({ status: 204, headers: { get: () => null }, text: async () => '' })

  function watch(url: string, opts: { remote?: string; focused?: boolean; answer?: string } = {}) {
    const t = setup(url)
    t.secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN })
    t.transport.route = ok204
    const clock = new FakeClock()
    const w = new StaleTokenWatch({ ...t.d, window: new FakeWindow(opts.remote, opts.focused ?? true), clock })
    t.prompter.message = async function (level, text, actions = []) {
      this.shown.push({ level, text, actions })
      return text.startsWith('drydock.url now names') ? opts.answer : undefined
    }
    return { ...t, clock, w }
  }

  it('typed a keystroke at a time, it asks nothing until the value holds still, then asks once', async () => {
    const t = watch(ORIGIN)
    const target = 'https://new.test'
    for (let i = 1; i <= target.length; i++) {
      t.settings.set(target.slice(0, i))
      t.w.changed()
      t.clock.advance(200)
      await flush()
    }
    expect(t.prompter.shown).toEqual([])
    expect(t.transport.requests).toEqual([])
    t.clock.advance(SETTLE_MS)
    await flush()
    expect(t.prompter.shown.map((s) => s.actions)).toEqual([[signOutOf(ORIGIN), KEEP]])
    expect(t.prompter.texts()[0]).toBe(staleSentence(ORIGIN, target))
    // Dismissed: nothing was sent, the token is kept.
    expect(t.transport.requests).toEqual([])
    expect(t.secrets.value).toContain(TOKEN)
  })

  it('an empty or invalid value does nothing; the same valid origin does nothing (controls for the prompt below)', async () => {
    for (const url of ['', 'https://', 'https://New.test', 'http://new.test', ORIGIN]) {
      const t = watch(url)
      expect(await t.w.check(), url).toBe('nothing')
      expect(t.prompter.shown, url).toEqual([])
      expect(t.transport.requests, url).toEqual([])
    }
  })

  it('a different valid origin asks once, however often it is checked', async () => {
    const t = watch('https://new.test')
    expect(await t.w.check()).toBe('kept')
    expect(await t.w.check()).toBe('nothing')
    t.w.changed()
    t.clock.advance(SETTLE_MS)
    await flush()
    expect(t.prompter.shown).toHaveLength(1)
  })

  it('Sign out revokes the token at its own origin and forgets it', async () => {
    const t = watch('https://new.test', { answer: signOutOf(ORIGIN) })
    expect(await t.w.check()).toBe('signed_out')
    expect(t.transport.requests.map((r) => [r.method, r.url, r.headers.Authorization])).toEqual(
      [['DELETE', `${ORIGIN}/api/auth/token`, `Bearer ${TOKEN}`]])
    expect(t.secrets.value).toBeUndefined()
  })

  it('Keep sends nothing and keeps the token', async () => {
    const t = watch('https://new.test', { answer: KEEP })
    expect(await t.w.check()).toBe('kept')
    expect(t.transport.requests).toEqual([])
    expect(t.secrets.value).toContain(TOKEN)
  })

  it('only the focused window, and only a local or SSH one, asks (the controls are the cases above)', async () => {
    for (const opts of [{ focused: false }, { remote: 'attached-container' }, { remote: 'dev-container' }]) {
      const t = watch('https://new.test', { ...opts, answer: signOutOf(ORIGIN) })
      expect(await t.w.check(), JSON.stringify(opts)).toBe('nothing')
      expect(t.prompter.shown).toEqual([])
      expect(t.transport.requests).toEqual([])
    }
    const ssh = watch('https://new.test', { remote: 'ssh-remote', answer: signOutOf(ORIGIN) })
    expect(await ssh.w.check()).toBe('signed_out')
  })

  it('a settle no focused window saw is asked when a window gains the focus; losing it asks nothing (control)', async () => {
    const t = watch('https://new.test')
    const win = new FakeWindow(undefined, false)
    const w = new StaleTokenWatch({ ...t.d, window: win, clock: t.clock })
    w.changed()
    t.clock.advance(SETTLE_MS)
    await flush()
    expect(t.prompter.shown).toEqual([])
    w.focusChanged(false)
    await flush()
    expect(t.prompter.shown).toEqual([])
    win.hasFocus = true
    w.focusChanged(true)
    await flush()
    expect(t.prompter.shown).toHaveLength(1)
  })

  it('gaining the focus while a settle is pending asks nothing until it fires', async () => {
    const t = watch('https://new.test')
    t.w.changed()
    t.w.focusChanged(true)
    await flush()
    expect(t.prompter.shown).toEqual([])
    t.clock.advance(SETTLE_MS)
    await flush()
    expect(t.prompter.shown).toHaveLength(1)
  })

  it('dispose cancels a pending settle', async () => {
    const t = watch('https://new.test')
    t.w.changed()
    expect(t.clock.pending()).toBe(1)
    t.w.dispose()
    expect(t.clock.pending()).toBe(0)
    t.clock.advance(SETTLE_MS)
    await flush()
    expect(t.prompter.shown).toEqual([])
  })

  it('a token replaced while the question was open is not the one signed out', async () => {
    const t = watch('https://new.test')
    t.prompter.message = async function (level, text, actions = []) {
      this.shown.push({ level, text, actions })
      t.secrets.value = JSON.stringify({ origin: 'https://new.test', token: TOKEN2 })
      return signOutOf(ORIGIN)
    }
    expect(await t.w.check()).toBe('nothing')
    expect(t.transport.requests).toEqual([])
    expect(t.secrets.value).toContain(TOKEN2)
  })
})
