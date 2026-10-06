import { describe, expect, it, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { ApiError, get, onUnauthorized, send } from './client'
import { freshBackend, server, useMockApi } from '../test/setup'

useMockApi()

describe('the 401 path in the client', () => {
  it('fires the handler on a 401 from any route, and not on a success', async () => {
    const b = freshBackend({ signedIn: true })
    const on401 = vi.fn()
    onUnauthorized(on401)

    // Positive control: a signed-in read succeeds and fires nothing.
    const s = await get<{ current: string }>('/api/auth/session')
    expect(s.current).not.toBe('')
    expect(on401).not.toHaveBeenCalled()

    // The session lapses. A read, a mutation, and a route nobody has heard of
    // all report it the same way.
    b.signedIn = false
    for (const call of [
      () => get('/api/auth/session'),
      () => send('POST', '/api/workspaces', { repo_id: 1 }),
      () => get('/api/no-such-route'),
    ]) {
      await expect(call()).rejects.toMatchObject({ status: 401, code: 'unauthenticated' })
    }
    expect(on401).toHaveBeenCalledTimes(3)
  })

  it('does not fire on a non-401 failure', async () => {
    freshBackend({ signedIn: true })
    const on401 = vi.fn()
    onUnauthorized(on401)
    // An unknown /api path while signed in is a JSON 404, not a sign-out.
    await expect(get('/api/no-such-route')).rejects.toMatchObject({ status: 404, code: 'not_found' })
    expect(on401).not.toHaveBeenCalled()
    // Control: the same handler does fire once the session is gone.
    await expect(send('DELETE', '/api/auth/session')).resolves.toBeUndefined()
    await expect(get('/api/no-such-route')).rejects.toMatchObject({ status: 401 })
    expect(on401).toHaveBeenCalledTimes(1)
  })
})

describe('mutations', () => {
  it('discard the response body, even when the server sends one', async () => {
    freshBackend()
    // A 202 carrying the workspace, as §5 describes. Applying it would be a
    // second write path for entity state (§4.2 step 3).
    server.use(http.post('/api/workspaces', () => HttpResponse.json({ id: 'ws1', state: 'cloning' }, { status: 202 })))
    const result = await send('POST', '/api/workspaces', { repo_id: 1 })
    expect(result).toBeUndefined()
    // Control: a read of the same shape does return it — the discard is
    // specific to mutations, not a client that returns nothing at all.
    server.use(http.get('/api/workspaces/ws1', () => HttpResponse.json({ id: 'ws1', state: 'cloning' })))
    await expect(get('/api/workspaces/ws1')).resolves.toEqual({ id: 'ws1', state: 'cloning' })
  })

  it('are same-origin JSON fetches, never no-cors', async () => {
    const b = freshBackend()
    await send('POST', '/api/auth/session', { password: 'drydock' })
    await get('/api/auth/session')
    const [post, read] = b.log
    expect(post).toMatchObject({ method: 'POST', credentials: 'same-origin', contentType: 'application/json' })
    expect(post!.mode).not.toBe('no-cors')
    expect(post!.url).toBe('https://drydock.test/api/auth/session')
    // Control: a read carries no body, so no Content-Type — the header above
    // is the client's doing, not the harness echoing a default.
    expect(read).toMatchObject({ method: 'GET', credentials: 'same-origin', contentType: null })
  })

  it("override the page's no-referrer policy, or Safari and Firefox send Origin: null", async () => {
    // Under the document's `no-referrer`, a non-GET fetch whose mode is not
    // `cors` gets `Origin: null` (Fetch, "append a request Origin header"),
    // which the server's exact match refuses. `referrerPolicy: 'same-origin'`
    // on the request is the fix, and `mode: 'same-origin'` is the guard that
    // keeps its Referer at Drydock. The browser tier proves the header; this
    // pins the init that produces it, for every method.
    freshBackend({ signedIn: true })
    const spy = vi.spyOn(globalThis, 'fetch')
    try {
      await get('/api/auth/session')
      for (const m of ['POST', 'PUT', 'PATCH', 'DELETE'] as const) {
        await send(m, '/api/x').catch(() => {})
      }
      expect(spy).toHaveBeenCalledTimes(5)
      for (const [, init] of spy.mock.calls) {
        expect(init).toMatchObject({ mode: 'same-origin', credentials: 'same-origin', referrerPolicy: 'same-origin' })
      }
      // Control: the spy sees the client's init as written, not a default —
      // the method differs per call, so these are the five requests above.
      expect(spy.mock.calls.map(([, i]) => i?.method)).toEqual(['GET', 'POST', 'PUT', 'PATCH', 'DELETE'])
    } finally {
      spy.mockRestore()
    }
  })

  it('refuse to leave the API', async () => {
    freshBackend({ signedIn: true })
    await expect(get('https://evil.example/api/x')).rejects.toThrow(/non-API path/)
    await expect(send('POST', '/signin')).rejects.toThrow(/non-API path/)
    // Control: an /api path goes through.
    await expect(get('/api/auth/session')).resolves.toBeTruthy()
  })
})

describe('the error envelope', () => {
  it('is read by code, with Retry-After, and an HTML body is not mistaken for one', async () => {
    freshBackend()
    server.use(
      http.post('/api/auth/session', () =>
        HttpResponse.json(
          { error: { code: 'locked_out', message: 'prose the UI must not depend on' } },
          { status: 429, headers: { 'Retry-After': '90' } },
        ),
      ),
      http.get('/api/html', () => HttpResponse.html('<!doctype html><title>Drydock</title>', { status: 200 })),
      http.get('/api/html404', () => HttpResponse.html('<!doctype html><p>not found</p>', { status: 404 })),
    )
    const err = await send('POST', '/api/auth/session', { password: 'x' }).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 429, code: 'locked_out', retryAfter: 90 })

    // The §3 rule-3 bug, seen from the client: an SPA fallback answering an
    // /api path with index.html. It must surface as unparseable, not as data.
    await expect(get('/api/html')).rejects.toMatchObject({ code: 'unparseable' })
    await expect(get('/api/html404')).rejects.toMatchObject({ status: 404, code: 'unparseable' })
  })

  it('reports a network failure as its own code', async () => {
    freshBackend()
    server.use(http.get('/api/down', () => HttpResponse.error()))
    await expect(get('/api/down')).rejects.toMatchObject({ status: 0, code: 'network' })
  })
})
