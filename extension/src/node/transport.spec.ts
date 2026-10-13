// The production transport against real HTTP servers (node:http on
// loopback): a redirect is returned as itself and never followed — its
// Location receives nothing, the token least of all — and a URL that
// parsing would change is refused before any connection. Plain HTTP here:
// what is under test is fetch's redirect and URL handling, which TLS does
// not change; the client tier repeats it through Caddy.

import { createServer, type IncomingMessage, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { afterEach, describe, expect, it } from 'vitest'
import { DrydockClient } from '../client'
import { CredentialStore } from '../credential'
import { RefusedURL, exactURL, nodeTransport } from './transport'
import { MemorySecrets, TOKEN } from '../../test/fakes'

interface Seen {
  method: string
  url: string
  authorization: string | undefined
}

const servers: Server[] = []
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => new Promise((r) => s.close(r))))
})

async function serve(answer: (req: IncomingMessage) => { status: number; headers?: Record<string, string>; body?: string }) {
  const seen: Seen[] = []
  const s = createServer((req, res) => {
    seen.push({ method: req.method ?? '', url: req.url ?? '', authorization: req.headers.authorization })
    const a = answer(req)
    res.writeHead(a.status, a.headers ?? {})
    res.end(a.body ?? '')
  })
  servers.push(s)
  await new Promise<void>((r) => s.listen(0, '127.0.0.1', r))
  return { origin: `http://127.0.0.1:${(s.address() as AddressInfo).port}`, seen }
}

describe('nodeTransport', () => {
  it('a 302 is returned as itself, and its Location receives no request; a 200 is the control', async () => {
    const target = await serve(() => ({ status: 200, headers: { 'Content-Type': 'application/json' }, body: '{}' }))
    const redirecting = await serve(() => ({ status: 302, headers: { Location: `${target.origin}/api/workspaces` } }))
    const headers = { Authorization: `Bearer ${TOKEN}`, Accept: 'application/json' }

    const resp = await nodeTransport.request({ method: 'GET', url: `${redirecting.origin}/api/workspaces`, headers })
    expect(resp.status).toBe(302)
    expect(redirecting.seen).toHaveLength(1)
    expect(target.seen).toEqual([])

    // The client turns it into an error, still without following it.
    const secrets = new MemorySecrets()
    secrets.value = JSON.stringify({ origin: redirecting.origin, token: TOKEN })
    const client = new DrydockClient({
      transport: nodeTransport, credentials: new CredentialStore(secrets), origin: () => ({ ok: true, origin: redirecting.origin }),
    })
    await expect(client.get('/api/workspaces')).rejects.toMatchObject({ status: 302, code: 'redirect' })
    expect(target.seen).toEqual([])

    // Control: the target answers a request sent to it, with the header.
    const ok = await nodeTransport.request({ method: 'GET', url: `${target.origin}/api/workspaces`, headers })
    expect(ok.status).toBe(200)
    expect(target.seen).toEqual([{ method: 'GET', url: '/api/workspaces', authorization: `Bearer ${TOKEN}` }])
  })

  it('the stream does not follow a redirect either', async () => {
    const target = await serve(() => ({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body: ': ping\n\n' }))
    const redirecting = await serve(() => ({ status: 307, headers: { Location: `${target.origin}/api/events` } }))
    const resp = await nodeTransport.stream({ method: 'GET', url: `${redirecting.origin}/api/events`, headers: { Authorization: `Bearer ${TOKEN}` } })
    resp.cancel()
    expect(resp.status).toBe(307)
    expect(target.seen).toEqual([])
  })

  it('a URL that parsing would change is refused before any connection; the exact one is sent (control)', async () => {
    const server = await serve(() => ({ status: 202 }))
    for (const path of [
      '/api/workspaces/%2e%2e/config-approval', '/api/workspaces/%2E%2E/config-approval', '/api/workspaces/.%2e/config-approval',
      '/api/workspaces/../config-approval', '/api/workspaces/./x', '/api/workspaces#f', '/api//workspaces/../x',
    ]) {
      await expect(nodeTransport.request({ method: 'POST', url: server.origin + path, headers: {} }), path).rejects.toBeInstanceOf(RefusedURL)
      await expect(nodeTransport.stream({ method: 'GET', url: server.origin + path, headers: {} }), path).rejects.toBeInstanceOf(RefusedURL)
    }
    expect(server.seen).toEqual([])
    const resp = await nodeTransport.request({ method: 'POST', url: `${server.origin}/api/workspaces/01JA0000000000000000000010/config-approval`, headers: {} })
    expect(resp.status).toBe(202)
    expect(server.seen.map((s) => s.url)).toEqual(['/api/workspaces/01JA0000000000000000000010/config-approval'])
  })
})

describe('exactURL', () => {
  it('refuses a user, a fragment, a capital host and an unparseable string; passes the exact form', () => {
    for (const u of ['https://u:p@drydock.test/api/meta', 'https://drydock.test/api/meta#x', 'https://Drydock.test/api/meta',
      'https://drydock.test:443/api/meta', 'not a url', 'https://drydock.test/api/%2e%2e/meta']) {
      expect(() => exactURL(u), u).toThrow(RefusedURL)
    }
    expect(exactURL('https://drydock.test:8443/api/workspaces?hidden=true')).toBe('https://drydock.test:8443/api/workspaces?hidden=true')
  })

  it('names only the path when it refuses, never the whole URL', () => {
    try {
      exactURL(`https://drydock.test/api/%2e%2e/meta?t=${TOKEN}`)
      expect.unreachable()
    } catch (e) {
      expect((e as Error).message).not.toContain(TOKEN)
    }
  })
})
