// The client's rules (client.ts; vscode-extension §4.2, §4.4, §8 rule 5,
// §12.2's origin binding), over a fake transport. Every negative assertion
// has its positive control in the same test.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../web/src/core/errors'
import { DrydockClient, NotConfigured, SignInNeeded, causeOf } from './client'
import { CredentialStore } from './credential'
import { originOf } from './settings'
import { FakeTransport, MemorySecrets, ORIGIN, TOKEN, errorResponse, jsonResponse, settingsOf } from '../test/fakes'

function setup(url = ORIGIN, stored: { origin: string; token: string } | null = { origin: ORIGIN, token: TOKEN }) {
  const transport = new FakeTransport()
  const secrets = new MemorySecrets()
  if (stored !== null) secrets.value = JSON.stringify(stored)
  const settings = settingsOf(url)
  let unauthorized = 0
  const client = new DrydockClient({
    transport, credentials: new CredentialStore(secrets), origin: () => originOf(settings.url()),
    onUnauthorized: () => { unauthorized++ },
  })
  return { transport, secrets, settings, client, unauthorized: () => unauthorized }
}

describe('the origin binding', () => {
  it('a credential stored for another origin sends nothing and asks for a sign-in; the same origin sends it (control)', async () => {
    for (const other of ['https://drydock.test:8443', 'https://other.test', 'http://drydock.test', 'https://Drydock.test']) {
      const { transport, client } = setup(ORIGIN, { origin: other, token: TOKEN })
      await expect(client.get('/api/workspaces')).rejects.toBeInstanceOf(SignInNeeded)
      expect(transport.requests).toEqual([])
    }
    const { transport, client } = setup()
    transport.route = () => jsonResponse(200, { workspaces: [] })
    await client.get('/api/workspaces')
    expect(transport.requests).toHaveLength(1)
    expect(transport.requests[0]!.url).toBe(`${ORIGIN}/api/workspaces`)
    expect(transport.requests[0]!.headers.Authorization).toBe(`Bearer ${TOKEN}`)
  })

  it('a changed drydock.url makes the stored token unsendable, without touching it', async () => {
    const { transport, client, settings, secrets } = setup()
    settings.set('https://elsewhere.test')
    await expect(client.get('/api/workspaces')).rejects.toMatchObject({ reason: 'other_origin' })
    expect(transport.requests).toEqual([])
    expect(secrets.value).toContain(TOKEN)
  })

  it('no credential or no drydock.url sends nothing', async () => {
    const none = setup(ORIGIN, null)
    await expect(none.client.get('/api/workspaces')).rejects.toMatchObject({ reason: 'none' })
    const unset = setup('')
    await expect(unset.client.get('/api/workspaces')).rejects.toBeInstanceOf(NotConfigured)
    expect([...none.transport.requests, ...unset.transport.requests]).toEqual([])
  })
})

describe('what is sent', () => {
  it('a mutation carries the configured origin as Origin; a read does not need it', async () => {
    const { transport, client } = setup('https://drydock.test:8443/', { origin: 'https://drydock.test:8443', token: TOKEN })
    transport.route = () => ({ status: 202, headers: { get: () => null }, text: async () => '{"id":"x"}' })
    await client.send('POST', '/api/workspaces', { repository_id: 1 })
    const req = transport.requests[0]!
    expect(req.headers.Origin).toBe('https://drydock.test:8443')
    expect(req.headers['Content-Type']).toBe('application/json')
    expect(req.body).toBe('{"repository_id":1}')
    transport.route = () => jsonResponse(200, {})
    await client.get('/api/repos')
    expect(transport.requests[1]!.headers.Origin).toBeUndefined()
  })

  it('a path no contracted route matches is refused before anything is sent; a contracted one goes (control)', async () => {
    const { transport, client } = setup()
    for (const [m, p] of [
      ['GET', '/api/secrets'], ['GET', '/api/tokens'], ['POST', '/api/tokens'], ['DELETE', '/api/auth/sessions'],
      ['GET', '/api/workspaces/a/b'], ['GET', '/api/workspaces/..'], ['GET', '//evil.test/api/meta'],
      ['GET', '@evil.test/api/meta'], ['POST', '/api/workspaces/x/start'], ['GET', '/api/workspaces#frag'],
    ] as const) {
      await expect(m === 'GET' ? client.get(p) : client.send(m, p)).rejects.toThrow(/refusing/)
    }
    expect(transport.requests).toEqual([])
    transport.route = () => jsonResponse(200, {})
    await client.get('/api/workspaces/01JA0000000000000000000010')
    expect(transport.requests).toHaveLength(1)
  })

  it("a mutation's body is never returned", async () => {
    const { transport, client } = setup()
    transport.route = () => jsonResponse(202, { id: '01J', state: 'pending' })
    expect(await client.send('POST', '/api/workspaces', { repository_id: 1 })).toBeUndefined()
  })
})

describe('what comes back', () => {
  it('a redirect is an error, never followed', async () => {
    const { transport, client } = setup()
    transport.route = () => ({ status: 302, headers: { get: (n: string) => (n === 'Location' ? 'https://evil.test/' : null) }, text: async () => '' })
    await expect(client.get('/api/workspaces')).rejects.toMatchObject({ status: 302, code: 'redirect' })
    expect(transport.requests).toHaveLength(1)
  })

  it('a 401 clears the stored token and runs the one 401 path; a 403 forbidden_scope and a 503 do not (control)', async () => {
    const forbidden = setup()
    forbidden.transport.route = () => errorResponse(403, 'forbidden_scope')
    await expect(forbidden.client.send('POST', '/api/workspaces/x/config-approval', { hash: 'h' })).rejects.toMatchObject({ code: 'forbidden_scope' })
    const busy = setup()
    busy.transport.route = () => errorResponse(503, 'unavailable', { 'Retry-After': '5' })
    await expect(busy.client.get('/api/workspaces')).rejects.toMatchObject({ code: 'unavailable', retryAfter: 5 })
    expect(forbidden.secrets.value).toContain(TOKEN)
    expect(busy.secrets.value).toContain(TOKEN)
    expect(forbidden.unauthorized() + busy.unauthorized()).toBe(0)

    const revoked = setup()
    revoked.transport.route = () => errorResponse(401, 'unauthenticated')
    await expect(revoked.client.get('/api/workspaces')).rejects.toMatchObject({ status: 401 })
    expect(revoked.secrets.value).toBeUndefined()
    expect(revoked.unauthorized()).toBe(1)
  })

  it('verifying a pasted token clears nothing on its 401', async () => {
    const { transport, client, secrets } = setup()
    transport.route = () => errorResponse(401, 'unauthenticated')
    await expect(client.metaWith(ORIGIN, 'drydock_' + 'q'.repeat(43))).rejects.toMatchObject({ status: 401 })
    expect(secrets.value).toContain(TOKEN)
    expect(transport.requests[0]!.headers.Authorization).toBe('Bearer drydock_' + 'q'.repeat(43))
  })

  it('a network failure is ApiError(0, network); a body that is not JSON is unparseable', async () => {
    const { transport, client } = setup()
    transport.route = () => {
      throw new TypeError(`fetch failed for ${ORIGIN}/api/workspaces?t=${TOKEN}`, { cause: Object.assign(new Error('connect ECONNREFUSED 10.0.0.1:443'), { code: 'ECONNREFUSED' }) })
    }
    const err = await client.get('/api/workspaces').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 0, code: 'network' })
    // The cause's name and code are kept for Show Log; its messages, which
    // quote a URL, are not.
    expect((err as ApiError).message).toBe('no response: TypeError Error ECONNREFUSED')
    expect((err as ApiError).message).not.toContain(TOKEN)
    transport.route = () => ({ status: 200, headers: { get: () => 'text/html' }, text: async () => '<html>' })
    await expect(client.get('/api/workspaces')).rejects.toMatchObject({ code: 'unparseable' })
  })
})

describe('causeOf', () => {
  it('keeps names and machine codes through the cause chain, and nothing else', () => {
    const e = new TypeError('fetch failed https://u:p@x.test/', {
      cause: Object.assign(new Error('unable to verify'), { code: 'UNABLE_TO_VERIFY_LEAF_SIGNATURE' }),
    })
    expect(causeOf(e)).toBe('no response: TypeError Error UNABLE_TO_VERIFY_LEAF_SIGNATURE')
    expect(causeOf(Object.assign(new Error('x'), { code: 'see https://evil.test/' + TOKEN }))).toBe('no response: Error')
    expect(causeOf('a string')).toBe('no response')
  })
})

describe('encoded dot segments', () => {
  it('a path whose segment is or encodes a dot segment, or a slash, is refused before anything is sent', async () => {
    const { transport, client } = setup()
    for (const p of [
      '/api/workspaces/%2e%2e/config-approval', '/api/workspaces/%2E%2E/config-approval', '/api/workspaces/.%2e/config-approval',
      '/api/workspaces/%2e./config-approval', '/api/workspaces/../config-approval', '/api/workspaces/./config-approval',
      '/api/workspaces/a%2Fb/config-approval', '/api/workspaces/%2e', '/api/workspaces/..?x=1', '/api/workspaces/%252e%252e',
    ]) {
      const send = p.endsWith('config-approval') ? client.send('POST', p, { hash: 'h' }) : client.get(p)
      await expect(send, p).rejects.toThrow(/refusing/)
    }
    expect(transport.requests).toEqual([])
    // Control: a ULID in the same place goes.
    await client.send('POST', '/api/workspaces/01JA0000000000000000000010/config-approval', { hash: 'h' })
    expect(transport.requests).toHaveLength(1)
  })
})

describe('the stream', () => {
  it('opens with the token, Accept and Last-Event-ID past 0, and a 401 clears the token', async () => {
    const { transport, client, secrets } = setup()
    await client.stream(0)
    await client.stream(42)
    expect(transport.streams[0]!.headers['Last-Event-ID']).toBeUndefined()
    expect(transport.streams[1]!.headers).toMatchObject({ 'Last-Event-ID': '42', Accept: 'text/event-stream', Authorization: `Bearer ${TOKEN}` })
    const { FakeStream } = await import('../test/fakes')
    const refused = new FakeStream(401)
    transport.streamRoute = () => refused
    await expect(client.stream(42)).rejects.toMatchObject({ status: 401 })
    expect(refused.cancelled).toBe(true)
    expect(secrets.value).toBeUndefined()
  })
})
