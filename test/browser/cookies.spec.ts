// The session cookie in a real browser (testing §8.1 "Cookie attributes",
// §10.2 item 4; Spike 04 B1–B3, C1–C4).
//
// The unit tier pins the exact Set-Cookie string. What only a browser can say
// is whether that string is *accepted* — a `__Host-` cookie that breaks one of
// the prefix's rules is silently dropped, and the symptom is "sign-in does
// nothing" — and whether it is replayed. Both are asserted at the server, via
// the tap, not inferred from a status code.

import { COOKIE, UI } from './harness'
import { expect, seenBy, sessionCookie, signIn, test } from './fixtures'

test('the real sign-in sets __Host-drydock with every attribute, and the browser replays it', async ({ page, context, stack }) => {
  await page.goto(`${UI}/signin`)

  // Before: no cookie in the jar, and a session probe arrives without one
  // and is refused.
  expect(await sessionCookie(context)).toBeUndefined()
  await page.evaluate(async () => {
    await fetch('/api/auth/session')
  })
  const probe = await seenBy(stack.apiTap.seen, (s) => s.path === '/api/auth/session' && s.method === 'GET', 'the probe')
  expect(probe.session).toBeNull()
  expect(probe.status).toBe(401)

  stack.apiTap.clear()
  expect(await signIn(page, stack.password)).toBe(204)
  const post = await seenBy(stack.apiTap.seen, (s) => s.method === 'POST' && s.path === '/api/auth/session', 'the sign-in POST')
  expect(String(post.responseHeaders?.['set-cookie'])).toContain(`${COOKIE}=`)

  // Accepted, with the attributes design §13.2 requires. Host-only: a
  // `__Host-` cookie may carry no Domain, so the jar records the exact host.
  const c = await sessionCookie(context)
  expect(c, 'the browser dropped the session cookie').toBeDefined()
  expect(c).toMatchObject({ domain: 'drydock.test', path: '/', secure: true, httpOnly: true, sameSite: 'Lax' })
  // HttpOnly, observed from the page rather than from the jar's flag.
  expect(await page.evaluate(() => document.cookie)).not.toContain(COOKIE)

  // Replayed on a same-origin request, as seen by the server.
  stack.apiTap.clear()
  const status = await page.evaluate(async () => (await fetch('/api/auth/session')).status)
  expect(status).toBe(200)
  const replay = await seenBy(stack.apiTap.seen, (s) => s.path === '/api/auth/session', 'the replay')
  expect(replay.session).toBe(c!.value)
})

test('the browser refuses the three illegal __Host- variants and keeps the legal one', async ({ page, context }) => {
  await page.goto(`${UI}/signin`)
  // Drydock never sends these, so the response is the browser's to judge
  // without a server that could be blamed: one response, four cookies, one
  // legal. The interception changes nothing about which origin the cookies
  // are for — the page is on the real https://drydock.test.
  await page.route(`${UI}/__variants`, (r) =>
    r.fulfill({
      status: 204,
      headers: {
        'set-cookie': [
          '__Host-ok=1; Secure; Path=/',
          '__Host-badpath=1; Secure; Path=/sub', // Path must be exactly /
          '__Host-nosecure=1; Path=/', // Secure is required
          '__Host-domain=1; Secure; Path=/; Domain=drydock.test', // Domain is forbidden
        ].join('\n'),
      },
    }),
  )
  await page.evaluate(async () => {
    await fetch('/__variants')
  })
  const names = (await context.cookies(UI)).map((c) => c.name)
  // The control: the legal variant was accepted, so cookies from this
  // response did reach the jar and the three absences below mean refusal.
  expect(names).toContain('__Host-ok')
  expect(names).not.toContain('__Host-badpath')
  expect(names).not.toContain('__Host-nosecure')
  expect(names).not.toContain('__Host-domain')
})
