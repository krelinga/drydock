// The client tier (vscode-extension §12.4): the extension's client, stream
// and session code, on its production transport, against the real Drydock
// behind real Caddy. Each negative assertion has its positive control in the
// same test, and "nothing was sent" is read off the tap between Caddy and
// Drydock's socket.

import { readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { ApiError } from '../../../web/src/core/errors'
import { vscodeLink, workspaceForRepo } from '../../../web/src/core/reducer'
import { cloneOverIn, settlesClone } from '../../../web/src/core/settle'
import type { WorkspaceDetail, WorkspaceList } from '../../../web/src/core/types'
import { DIND, ExtensionStack, PLAIN, UI_HOST } from '../../../test/extension/stack'
import { SignInNeeded } from '../../src/client'
import { parseMeta } from '../../src/handshake'
import { remoteURI } from '../../src/link'
import { nodeTransport } from '../../src/node/transport'
import { PASTE_TOKEN, signIn, signOut, type SessionDeps } from '../../src/session'
import { SseParser } from '../../src/sse'
import { MemoryLog, RecordingOpener, ScriptedPrompter, settingsOf } from '../fakes'
import { Follower, bound, until } from './world'

const REPO = path.resolve(process.cwd(), '..')
let stack: ExtensionStack

beforeAll(async () => {
  const certs = process.env.DRYDOCK_EXTENSION_CERTS
  if (!certs) throw new Error('no DRYDOCK_EXTENSION_CERTS: run this tier with test/extension/run.sh')
  stack = await ExtensionStack.start(REPO, certs)
})
afterAll(async () => {
  await stack?.teardown()
})

// What the tap saw, with each Authorization value replaced by its label, so
// a failing assertion prints no token.
const fromTap = () => stack.tap.seen.map((s) => ({ method: s.method, path: s.path, authorization: stack.bearerLabel(s.authorization), origin: s.origin }))
/** Whether the stored entry is exactly {origin, token}, without printing it. */
const holds = (value: string | undefined, origin: string, token: string) => {
  if (value === undefined) return false
  const v = JSON.parse(value) as { origin?: string; token?: string }
  return v.origin === origin && v.token === token
}

describe('sign-in and the handshake', () => {
  it('a token minted in Settings is pasted, verified with GET /api/meta and stored for this origin; the client never signs in with the password', async () => {
    const { token } = await stack.mintToken(['read', 'lifecycle'], 'VS Code on the client tier')
    const b = bound(stack.origin, null)
    const prompter = new ScriptedPrompter()
    prompter.answers = [PASTE_TOKEN]
    prompter.typed = [token]
    const d: SessionDeps = {
      prompter, opener: new RecordingOpener(), settings: settingsOf(stack.origin), credentials: b.credentials, client: b.client, log: new MemoryLog(),
    }
    stack.tap.clear()
    const meta = await signIn(d)
    expect(meta).toMatchObject({ version: 'dev', api: 1, credential: { kind: 'token', label: 'VS Code on the client tier', scopes: ['read', 'lifecycle', 'self'] } })
    expect(holds(b.secrets.value, stack.origin, token)).toBe(true)
    expect(fromTap()).toEqual([{ method: 'GET', path: '/api/meta', authorization: stack.labelOf(token), origin: null }])
    expect(stack.tap.seen.some((s) => s.path.startsWith('/api/auth/session'))).toBe(false)
  })

  it('GET /api/meta answers the stack\'s version and api', async () => {
    const { token } = await stack.mintToken(['read'])
    const meta = parseMeta(await bound(stack.origin, token).client.get('/api/meta'))
    expect(meta).toMatchObject({ version: 'dev', api: 1 })
  })
})

describe('the gate, as the client meets it', () => {
  it('a mutation without Origin is forbidden_origin; the client, which always sends it, is accepted (control)', async () => {
    const { token } = await stack.mintToken(['read', 'lifecycle'])
    const bare = await fetch(`${stack.origin}/api/repos/refresh`, { method: 'POST', headers: { Authorization: `Bearer ${token}` } })
    expect(bare.status).toBe(403)
    expect(((await bare.json()) as { error: { code: string } }).error.code).toBe('forbidden_origin')
    await bound(stack.origin, token).client.send('POST', '/api/repos/refresh')
    expect(stack.tap.seen.at(-1)).toMatchObject({ method: 'POST', path: '/api/repos/refresh', origin: stack.origin, status: 202 })
  })

  it('a cookie together with any Authorization header is 401; either alone is 200 (control)', async () => {
    const { token } = await stack.mintToken(['read'])
    const cookie = `__Host-drydock=${await stack.sessionCookie()}`
    const get = (h: Record<string, string>) => fetch(`${stack.origin}/api/workspaces`, { headers: h }).then((r) => r.status)
    expect(await get({ Cookie: cookie, Authorization: `Bearer ${token}` })).toBe(401)
    expect(await get({ Cookie: cookie, Authorization: 'Basic eDp5' })).toBe(401)
    expect(await get({ Cookie: cookie })).toBe(200)
    expect(await get({ Authorization: `Bearer ${token}` })).toBe(200)
  })
})

describe('the token goes only where it was stored for', () => {
  it('a client configured for a second Caddy site, holding the first site\'s token, sends none; the storing origin gets it (control)', async () => {
    const { token } = await stack.mintToken(['read'])
    const second = await stack.otherCaddy('second')
    try {
      const secondOrigin = `https://${UI_HOST}:${second.httpsPort}`
      const b = bound(secondOrigin, token, stack.origin)
      stack.tap.clear()
      await expect(b.client.get('/api/workspaces')).rejects.toBeInstanceOf(SignInNeeded)
      expect(stack.tap.seen).toEqual([])
      b.setOrigin(stack.origin)
      await b.client.get('/api/workspaces')
      expect(fromTap()).toEqual([{ method: 'GET', path: '/api/workspaces', authorization: stack.labelOf(token), origin: null }])
    } finally {
      await second.stop()
    }
  })

  it('a redirect is returned, never followed: Caddy\'s HTTP port answers 3xx and nothing reaches Drydock', async () => {
    const { token } = await stack.mintToken(['read'])
    stack.tap.clear()
    const r = await nodeTransport.request({
      method: 'GET', url: `http://${UI_HOST}:${stack.caddy.httpPort}/api/workspaces`, headers: { Authorization: `Bearer ${token}` },
    })
    expect(r.status).toBeGreaterThanOrEqual(300)
    expect(r.status).toBeLessThan(400)
    expect(r.headers.get('Location')).toMatch(/^https:/)
    expect(stack.tap.seen).toEqual([])
    // Control: the same request at the HTTPS origin reaches Drydock, with the token.
    const ok = await nodeTransport.request({
      method: 'GET', url: `${stack.origin}/api/workspaces`, headers: { Authorization: `Bearer ${token}` },
    })
    expect(ok.status).toBe(200)
    expect(fromTap()).toEqual([{ method: 'GET', path: '/api/workspaces', authorization: stack.labelOf(token), origin: null }])
  })

  it('a certificate for another name is refused with TLS validation on, and nothing is sent; the right one is accepted (control)', async () => {
    const { token } = await stack.mintToken(['read'])
    const wrong = await stack.otherCaddy('wrongcert', { uiCert: stack.otherCert() })
    try {
      const wrongOrigin = `https://${UI_HOST}:${wrong.httpsPort}`
      stack.tap.clear()
      const err = await bound(wrongOrigin, token).client.get('/api/workspaces').catch((e: unknown) => e)
      expect(err).toBeInstanceOf(ApiError)
      expect(err).toMatchObject({ status: 0, code: 'network' })
      expect((err as ApiError).message).toContain('ERR_TLS_CERT_ALTNAME_INVALID')
      expect(stack.tap.seen).toEqual([])
      await bound(stack.origin, token).client.get('/api/workspaces')
      expect(stack.tap.seen).toHaveLength(1)
    } finally {
      await wrong.stop()
    }
  })
})

describe('the stream', () => {
  it('a bearer stream, live and replayed, carries no secret.* or api_token.* event and not the secret\'s name; a cookie stream over the same window has them (control)', async () => {
    const { token } = await stack.mintToken(['read', 'lifecycle'])
    const b = bound(stack.origin, token)
    const f = new Follower(b.client).start()
    const cookie = await cookieStream(stack.origin, await stack.sessionCookie())
    try {
      await until('both streams open', () => f.opens === 1 && cookie.open)
      const before = f.entities.lastEventId
      const canary = `CANARY_${Date.now().toString(36).toUpperCase()}`
      await b.client.send('POST', '/api/repos/refresh')
      const put = await stack.direct('PUT', `/api/secrets/${canary}`, { value: 'x', reach: 'Nothing: a test canary.', description: '' })
      expect(put.status).toBeLessThan(300)
      await stack.mintToken(['read'], `token-${canary}`)
      await b.client.send('POST', '/api/repos/refresh')
      await until('the cookie stream sees all four', () =>
        cookie.kinds().filter((k) => k === 'repo.refreshed').length >= 2 && cookie.kinds().includes('secret.created') &&
        cookie.kinds().includes('api_token.created'))
      await until('the bearer stream sees both refreshes', () => f.kinds().filter((k) => k === 'repo.refreshed').length >= 2)
      const hidden = (k: string) => k.startsWith('secret.') || k.startsWith('api_token.')
      expect(f.kinds().filter(hidden)).toEqual([])
      expect(JSON.stringify(f.events)).not.toContain(canary)
      expect(cookie.raw).toContain(canary)

      // Replayed: a new bearer stream resuming from before the secret.
      const replay = await rawStream(stack.origin, { Authorization: `Bearer ${token}`, 'Last-Event-ID': String(before) })
      await until('the replay reaches the second refresh', () => replay.kinds().filter((k) => k === 'repo.refreshed').length >= 2)
      replay.close()
      expect(replay.kinds().filter(hidden)).toEqual([])
      expect(replay.raw).not.toContain(canary)
      // The hidden events still moved the position: their ids came as id-only frames.
      expect(replay.idOnly).toBeGreaterThanOrEqual(2)
    } finally {
      f.stop()
      cookie.close()
    }
  })

  it('across a Caddy restart, events written meanwhile arrive once each, in order, by Last-Event-ID', async () => {
    const { token } = await stack.mintToken(['read', 'lifecycle'])
    const f = new Follower(bound(stack.origin, token).client).start()
    try {
      await until('open', () => f.opens === 1)
      await bound(stack.origin, token).client.send('POST', '/api/repos/refresh')
      await until('the first refresh arrives', () => f.kinds().includes('repo.refreshed'))
      const seenBefore = f.events.length
      // Written while Caddy is down, one after the other: each refresh is
      // waited for on Drydock's own socket before the next is asked, since
      // the catalog coalesces a refresh asked while one runs.
      const watch = await stack.socketEvents()
      try {
        await stack.caddy.kill()
        for (let i = 1; i <= 2; i++) {
          const r = await stack.direct('POST', '/api/repos/refresh')
          expect(r.status).toBe(202)
          await until(`refresh ${i} written`, () => watch.count('repo.refreshed') >= i)
        }
      } finally {
        watch.close()
      }
      await stack.caddy.run()
      await until('the two written meanwhile arrive', () => f.events.slice(seenBefore).filter((e) => e.kind === 'repo.refreshed').length >= 2, 60_000)
      const ids = f.events.map((e) => e.id)
      expect(new Set(ids).size).toBe(ids.length)
      expect([...ids].sort((a, c) => a - c)).toEqual(ids)
      expect(f.opens).toBeGreaterThanOrEqual(2)
      expect(stack.tap.seen.filter((s) => s.path === '/api/events').at(-1)!.lastEventId).not.toBeNull()
    } finally {
      f.stop()
    }
  })

  it('a revoked token: the next request is 401 and clears it; the stream\'s reconnect ends on the 401 path (Sign Out, the control, revokes its own)', async () => {
    const { id, token } = await stack.mintToken(['read'])
    const b = bound(stack.origin, token)
    const f = new Follower(b.client).start()
    try {
      await until('open', () => f.opens === 1)
      expect((await stack.direct('DELETE', `/api/tokens/${id}`)).status).toBeLessThan(300)
      // Drop the connection rather than wait out a heartbeat (the server's
      // own tests hold Alive to one): the reconnect meets the 401.
      await stack.caddy.kill()
      await stack.caddy.run()
      await until('the stream ends on the 401', () => f.ended === 'unauthorized', 30_000)
      expect(b.secrets.value === undefined).toBe(true)
      expect(b.unauthorized()).toBe(1)
    } finally {
      f.stop()
    }

    // Sign Out: the token revokes itself, and is then refused.
    const other = await stack.mintToken(['read'])
    const o = bound(stack.origin, other.token)
    const prompter = new ScriptedPrompter()
    await signOut({ prompter, opener: new RecordingOpener(), settings: settingsOf(stack.origin), credentials: o.credentials, client: o.client, log: new MemoryLog() })
    expect(o.secrets.value === undefined).toBe(true)
    expect(stack.tap.seen.at(-1)).toMatchObject({ method: 'DELETE', path: '/api/auth/token', status: 204 })
    expect((await stack.direct('GET', '/api/meta', undefined, { bearer: other.token })).status).toBe(401)
  })
})

describe('clone, follow, connect', () => {
  it('POST /api/workspaces, the mark ends on settlesClone, the reducer reaches running, and the refetched view carries a link the extension can open', async () => {
    const { token } = await stack.mintToken(['read', 'lifecycle'])
    const b = bound(stack.origin, token)
    const f = new Follower(b.client).start()
    try {
      await until('open', () => f.opens === 1)
      const at = f.entities.lastEventId
      f.dispatch({ type: 'workspaces', at, view: await b.client.get<WorkspaceList>('/api/workspaces') })
      const key = `repo:${PLAIN.id}:clone`
      f.marks.begin(key, settlesClone(PLAIN.id), cloneOverIn(PLAIN.id))
      expect(await b.client.send('POST', '/api/workspaces', { repository_id: PLAIN.id })).toBeUndefined()
      f.marks.accepted(key)
      await until('the clone mark ends', () => !f.marks.has(key))
      // The workspace id is learned from the stream, never the 202.
      const w0 = workspaceForRepo(f.entities, PLAIN.id)
      expect(w0).not.toBeNull()
      await until('running', () => workspaceForRepo(f.entities, PLAIN.id)?.state === 'running', 60_000)
      const id = w0!.id
      const tag = f.entities.lastEventId
      f.dispatch({ type: 'workspace', at: tag, view: await b.client.get<WorkspaceDetail>(`/api/workspaces/${id}`) })
      const link = vscodeLink(f.entities.workspaces[id]!)
      expect(link).not.toBeNull()
      expect(remoteURI(link)).toBe(link!.replace('vscode://vscode-remote/', 'vscode-remote://'))
      expect(remoteURI(link)).toMatch(/^vscode-remote:\/\/attached-container\+[0-9a-f]+@ssh-remote\+devbox\/workspaces\/plain$/)

      // Real payloads, recorded for the unit tier (§12.4).
      const kinds = new Set(f.kinds())
      expect(kinds.has('workspace.state')).toBe(true)
      writeFileSync(path.join(stack.root, 'recorded-events.json'), JSON.stringify(f.events, null, 1))
      expect(JSON.parse(readFileSync(path.join(stack.root, 'recorded-events.json'), 'utf8')).length).toBeGreaterThan(5)
    } finally {
      f.stop()
    }
  })
})

describe('host-access approval, as the API does it', () => {
  it('a docker-in-docker clone stops asking for privileged; a token without approve is forbidden_scope and kept; a stale hash is approval_stale; the shown hash is approved and runs', async () => {
    const lifecycle = await stack.mintToken(['read', 'lifecycle'])
    const approver = await stack.mintToken(['read', 'lifecycle', 'approve'])
    const b = bound(stack.origin, lifecycle.token)
    const a = bound(stack.origin, approver.token)
    const f = new Follower(b.client).start()
    try {
      await until('open', () => f.opens === 1)
      stack.dindVariant('privileged')
      await b.client.send('POST', '/api/workspaces', { repository_id: DIND.id })
      await until('stopped for approval', () => workspaceForRepo(f.entities, DIND.id)?.approval != null, 60_000)
      const w = workspaceForRepo(f.entities, DIND.id)!
      const hashA = w.approval!.hash
      expect(w.state).toBe('stopped')
      expect(w.approval!.added.map((s) => s.field)).toContain('privileged')
      const approve = `/api/workspaces/${w.id}/config-approval`

      // Without approve: 403, and the token is kept.
      await expect(b.client.send('POST', approve, { hash: hashA })).rejects.toMatchObject({ status: 403, code: 'forbidden_scope' })
      expect(holds(b.secrets.value, stack.origin, lifecycle.token)).toBe(true)

      // The request changes: declined, the configuration grows an
      // initializeCommand, started again — request B.
      expect((await stack.direct('DELETE', approve)).status).toBeLessThan(300)
      await until('declined', () => f.entities.workspaces[w.id]?.approval === null)
      stack.dindVariant('initialize')
      expect((await stack.direct('POST', `/api/workspaces/${w.id}/start`)).status).toBe(202)
      await until('request B', () => {
        const r = f.entities.workspaces[w.id]?.approval
        return r != null && r.hash !== hashA
      }, 60_000)
      const hashB = f.entities.workspaces[w.id]!.approval!.hash
      expect(f.entities.workspaces[w.id]!.approval!.added.map((s) => s.field)).toContain('initializeCommand')

      // A, no longer the request: refused, nothing recorded.
      await expect(a.client.send('POST', approve, { hash: hashA })).rejects.toMatchObject({ status: 409, code: 'approval_stale' })
      expect(f.kinds()).not.toContain('config.approved')

      // B, the one shown: approved, the same hash recorded, and it runs.
      await a.client.send('POST', approve, { hash: hashB })
      await until('config.approved', () => f.kinds().includes('config.approved'))
      expect(f.events.find((e) => e.kind === 'config.approved')!.data).toMatchObject({ hash: hashB })
      await until('running', () => f.entities.workspaces[w.id]?.state === 'running', 60_000)
    } finally {
      f.stop()
    }
  })
})

interface RawStream {
  raw: string
  open: boolean
  idOnly: number
  kinds(): string[]
  close(): void
}

/** A stream read by hand (the cookie control; a replay with a chosen Last-Event-ID), parsed by the extension's parser. */
async function rawStream(origin: string, headers: Record<string, string>): Promise<RawStream> {
  const ac = new AbortController()
  const resp = await fetch(`${origin}/api/events`, { headers: { Accept: 'text/event-stream', ...headers }, signal: ac.signal })
  const out: RawStream & { events: string[] } = {
    raw: '', open: resp.status === 200, idOnly: 0, events: [],
    kinds() {
      return this.events
    },
    close: () => ac.abort(),
  }
  if (resp.status !== 200 || resp.body === null) throw new Error(`stream: ${resp.status}`)
  const parser = new SseParser()
  const decoder = new TextDecoder()
  void (async () => {
    try {
      for await (const chunk of resp.body!) {
        const text = decoder.decode(chunk as Uint8Array, { stream: true })
        out.raw += text
        for (const fr of parser.feed(text)) {
          if (fr.kind === 'id') out.idOnly++
          if (fr.kind === 'event' && fr.type === 'message') {
            try {
              out.events.push((JSON.parse(fr.data) as { kind: string }).kind)
            } catch {
              /* skip */
            }
          }
        }
      }
    } catch {
      /* aborted */
    }
  })()
  return out
}

function cookieStream(origin: string, session: string): Promise<RawStream> {
  return rawStream(origin, { Cookie: `__Host-drydock=${session}` })
}
