// The event stream in a real EventSource, through the real Caddy (testing
// §10.2 items 9 and 13).
//
// The component tier already shows Caddy flushes an SSE frame to curl. Two
// things only a browser adds: it asks for compression (Accept-Encoding: gzip,
// br, zstd — and the UI site block has `encode zstd gzip`), so this is the
// path where a buffering encoder would bite; and its EventSource reconnects on
// its own and sends Last-Event-ID, which is browser behaviour, not code of ours.

import { randomBytes } from 'node:crypto'
import { UI } from './harness'
import { expect, putSecret, seenBy, sessionCookie, test } from './fixtures'

const unique = (p: string) => `${p}_${randomBytes(4).toString('hex').toUpperCase()}`

test('an event reaches a real EventSource through Caddy while the stream stays open', async ({ signedIn: page, stack }) => {
  const response = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/events')
  // A bare EventSource, so what is measured is the transport, not the app.
  await page.evaluate(() => {
    const w = window as unknown as { got: string[]; es: EventSource; opened: Promise<void> }
    w.got = []
    w.es = new EventSource('/api/events')
    w.es.onmessage = (m) => w.got.push(m.data as string)
    w.opened = new Promise((r) => w.es.addEventListener('open', () => r(), { once: true }))
  })
  await page.evaluate(() => (window as unknown as { opened: Promise<void> }).opened)
  const stream = await seenBy(stack.apiTap.seen, (s) => s.path === '/api/events', 'the stream request')
  // The browser asked for compression, so Caddy's `encode` is in the path.
  expect(stream.acceptEncoding).toMatch(/zstd|gzip/)
  const encoding = (await response).headers()['content-encoding'] ?? 'identity'
  test.info().annotations.push({ type: 'content-encoding', description: encoding })

  const name = unique('SSE')
  const t0 = Date.now()
  expect(await putSecret(page, name)).toBe(200)
  await expect
    .poll(() => page.evaluate(() => (window as unknown as { got: string[] }).got.join('\n')), { timeout: 5_000 })
    .toContain(`"kind":"secret.created"`)
  const latency = Date.now() - t0
  // Delivered while the response is still open — the stream never ended,
  // so nothing could have been waiting for a body to finish.
  expect(await page.evaluate(() => (window as unknown as { es: EventSource }).es.readyState)).toBe(1) // EventSource.OPEN
  expect(await page.evaluate((n) => (window as unknown as { got: string[] }).got.some((d) => d.includes(n)), name)).toBe(true)
  // Well inside the 20 s heartbeat: a buffer flushed by the next ping would
  // also eventually "arrive", and that is the failure this bounds.
  expect(latency).toBeLessThan(5_000)
})

test("a reconnect replays the gap through the browser's own Last-Event-ID", async ({ signedIn: page, context, stack }) => {
  const cookie = (await sessionCookie(context))!.value
  const before = unique('GAP_BEFORE')
  expect(await putSecret(page, before)).toBe(200)

  await page.goto(`${UI}/secrets`)
  const names = page.locator('[data-test=secret-name]')
  await expect(names.filter({ hasText: before })).toHaveCount(1)
  // Control: the live stream is delivering. A secret created now appears
  // without anyone reloading.
  const live = unique('GAP_LIVE')
  expect(await putSecret(page, live)).toBe(200)
  await expect(names.filter({ hasText: live })).toHaveCount(1)

  // From here the page may learn about secrets ONLY from the stream: its
  // backstop refetch (which runs on every reopen) is cut off. Otherwise the
  // list would be repaired by the refetch and the replay would go untested.
  let refetches = 0
  await page.route(`${UI}/api/secrets`, (r) => {
    refetches++
    return r.abort()
  })

  // Kill the stream: Caddy dies, every connection through it drops, and the
  // browser's EventSource goes back to CONNECTING on its own.
  stack.apiTap.clear()
  await stack.caddy.kill()

  // The gap: three events written while the browser cannot hear them.
  const gap = [unique('GAP_A'), unique('GAP_B'), unique('GAP_C')]
  for (const n of gap) {
    const r = await stack.direct('PUT', `/api/secrets/${n}`, cookie, { value: 'v', reach: 'nothing: a fixture', description: '' })
    expect(r.status, r.body).toBe(200)
  }

  await stack.caddy.run()
  // The browser reconnected by itself and said where it got to — the header,
  // which only the browser's own reconnect sends; the app's hard retry would
  // put it in the query instead.
  const re = await seenBy(stack.apiTap.seen, (s) => s.path.startsWith('/api/events') && s.status === 200, 'the reconnect', 30_000)
  expect(re.lastEventId).toMatch(/^\d+$/)
  expect(re.path).toBe('/api/events')

  for (const n of gap) await expect(names.filter({ hasText: n })).toHaveCount(1)
  // The refetch was attempted and refused, so what filled the gap was replay.
  expect(refetches).toBeGreaterThan(0)

  // And the page agrees with the database.
  const db = JSON.parse((await stack.direct('GET', '/api/secrets', cookie)).body) as { secrets: { name: string }[] }
  const onPage = (await names.allTextContents()).map((s) => s.trim()).sort()
  expect(onPage).toEqual(db.secrets.map((s) => s.name).sort())
})
