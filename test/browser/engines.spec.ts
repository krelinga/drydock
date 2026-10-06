// The UI's Origin in every engine (frontend §3, §8; design §13.3). Run by the
// chromium, firefox and webkit projects alike.
//
// v0.2.1 shipped a fetch client whose every mutation reached the server as
// `Origin: null` in Safari: the page's `no-referrer` policy, applied to a
// non-GET fetch whose mode is not `cors`, serializes the Origin as `null`
// (Fetch, "append a request Origin header"), and the server's exact match
// refuses it with `forbidden_origin`. Chromium sends the real origin either
// way, and this tier was Chromium-only, so it passed. Playwright's WebKit and
// Firefox both reproduce it.
//
// These tests drive the real sign-in form and a real Settings button, so the
// request is made by the app's own client, not a fetch written here — a bare
// `fetch()` defaults to mode `cors` and sends the right Origin whatever the
// client does. They run against the Stack's loopback front (harness.ts), the
// real Drydock over plain HTTP on 127.0.0.1, because the TLS stack's CA can
// only be trusted by Chromium here. The Origin rule under test is the same
// over HTTP and HTTPS: neither `no-referrer` nor `same-origin` turns on the
// scheme for a same-scheme request.

import http from 'node:http'
import { expect, seenBy, test } from './fixtures'

test("the app's own requests carry the page's exact Origin: sign-in and sign-out", async ({ page, stack, browserName }) => {
  const front = await stack.front()
  front.tap.clear()

  // Controls on the front itself, first: it passes a `null` Origin through
  // and the server refuses it, and it maps its own origin so the server's
  // check passes (a wrong password is then the 401 it should be, not a 403).
  // So the successes below are the browser's Origin being right, not the
  // front laundering a wrong one.
  expect(await post(front.url, 'null')).toBe(403)
  expect(await post(front.url, `${front.url}/`)).toBe(403) // exact: not a prefix
  expect(await post(front.url, front.url)).toBe(401)

  await page.goto(`${front.url}/signin`)
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()
  front.tap.clear()

  // The real form signs in. What the browser sent is the page's exact origin,
  // and the server took it.
  await page.getByLabel('Password').fill(stack.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  const signIn = await seenBy(front.tap.seen, (s) => s.method === 'POST' && s.path === '/api/auth/session', 'the sign-in POST')
  expect(signIn.origin, `${browserName}: the Origin of the form's sign-in`).toBe(front.url)
  expect(signIn.status).toBe(204)
  await expect(page).toHaveURL(`${front.url}/`)
  await expect(page.getByRole('heading', { name: 'Workspaces' })).toBeVisible()

  // A second mutation, by another view: Settings' sign-out, a DELETE.
  await page.goto(`${front.url}/settings`)
  await page.locator('[data-test=sign-out]').click()
  const signOut = await seenBy(front.tap.seen, (s) => s.method === 'DELETE' && s.path === '/api/auth/session', 'the sign-out DELETE')
  expect(signOut.origin, `${browserName}: the Origin of Settings' sign-out`).toBe(front.url)
  expect(signOut.status).toBe(204)
  await expect(page).toHaveURL(/\/signin(\?|$)/)

  // The fix's cost, bounded: the request's `same-origin` referrer policy
  // sends a Referer, and it is Drydock's own page, sent to Drydock — the
  // client's `mode: 'same-origin'` allows no other destination.
  for (const s of [signIn, signOut]) {
    expect(s.referer?.startsWith(`${front.url}/`), `${browserName}: ${s.method}'s Referer ${s.referer}`).toBe(true)
  }
})

/** A wrong-password sign-in through the front, from Node, with this Origin. */
function post(base: string, origin: string): Promise<number> {
  const u = new URL('/api/auth/session', base)
  const body = JSON.stringify({ password: 'not-the-password' })
  return new Promise((resolve, reject) => {
    const req = http.request(u, {
      method: 'POST',
      headers: { Origin: origin, 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
    }, (res) => {
      res.resume()
      resolve(res.statusCode ?? 0)
    })
    req.on('error', reject)
    req.end(body)
  })
}
