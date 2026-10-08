// The preview front door in a browser (port forwarding §13 step 1; testing
// §10.4): every preview URL answers one 401, the UI's session cookie never
// reaches the preview socket, and what the preview mux will authenticate on —
// a host-only preview cookie, a ?t= token — does reach it through the real
// Caddy, and changes nothing.
//
// Assertions about cookies are made at the server, through the tap, as in
// crosssite.spec.ts.

import { COOKIE, OTHER_PREVIEW_HOST, PREVIEW, PREVIEW_HOST, UI } from './harness'
import { expect, seenBy, sessionCookie, test } from './fixtures'

/** Adds a link to the page and clicks it: a real, user-initiated navigation. */
async function clickTo(page: import('@playwright/test').Page, href: string): Promise<void> {
  await page.evaluate((u) => {
    const a = document.createElement('a')
    a.href = u
    a.id = 'go'
    a.textContent = 'go'
    document.body.append(a)
  }, href)
  await page.click('#go')
}

test('a preview origin answers 401, and a signed-in browser sends it no UI cookie', async ({ signedIn: page, context, stack }) => {
  const cookie = (await sessionCookie(context))!.value

  // Control: a click to the UI origin carries the session, and the API reads it.
  await clickTo(page, `${UI}/api/auth/session?control=1`)
  const control = await seenBy(stack.apiTap.seen, (s) => s.path === '/api/auth/session?control=1', 'the control navigation')
  expect(control.session).toBe(cookie)
  expect(control.status).toBe(200)

  // The same signed-in browser, clicking through to a preview: the request
  // arrives at the preview socket, cross-site, without the session cookie,
  // and is refused.
  await page.goto(`${UI}/signin`)
  const [nav] = await Promise.all([page.waitForNavigation(), clickTo(page, `${PREVIEW}/api/auth/session?from-ui=1`)])
  const seen = await seenBy(stack.previewTap.seen, (s) => s.path === '/api/auth/session?from-ui=1', 'the preview navigation')
  expect(seen.host).toBe(PREVIEW_HOST)
  expect(seen.secFetchSite).toBe('cross-site')
  expect(seen.session).toBeNull()
  expect(seen.cookieNames).not.toContain(COOKIE)
  expect(seen.status).toBe(401)
  expect(nav?.status()).toBe(401)
  // Nothing reached the API socket: a preview URL with an API path is not an
  // API route.
  expect(stack.apiTap.seen.filter((s) => s.path.includes('from-ui'))).toHaveLength(0)

  // The answer is the front door's, not the UI's: no app, no UI CSP, no
  // cookie set, and no-referrer.
  const h = seen.responseHeaders!
  expect(h['set-cookie']).toBeUndefined()
  expect(h['content-security-policy']).toBeUndefined()
  expect(h['referrer-policy']).toBe('no-referrer')
  expect(h['cache-control']).toBe('no-store')
  expect(await page.content()).not.toContain('id="app"')
  expect(await page.content()).toContain('unauthenticated')
})

test('a preview cookie and a token reach the preview socket through Caddy, and get the same 401 as nothing', async ({ page, context, stack }) => {
  const plain = await page.goto(`${PREVIEW}/`)
  expect(plain?.status()).toBe(401)
  const want = await plain!.text()

  // A forged host-only preview cookie, the shape step 2 will set (PF §7):
  // Secure, HttpOnly, SameSite=Lax, on this one preview host.
  await context.addCookies([
    { name: 'drydock-preview', value: 'forged', url: `${PREVIEW}/`, secure: true, httpOnly: true, sameSite: 'Lax' },
  ])
  for (const path of ['/', '/.drydock/session?t=forged', '/.drydock/denied', '/api/repos', '/preview/authorize']) {
    stack.previewTap.clear()
    const resp = await page.goto(`${PREVIEW}${path}`)
    const seen = await seenBy(stack.previewTap.seen, (s) => s.path === path, `GET ${path}`)
    // The cookie arrived: Caddy's hop passes it through (PF §9), so the
    // preview mux can authenticate on it once it can.
    expect(seen.cookieNames, path).toContain('drydock-preview')
    expect(resp?.status(), path).toBe(401)
    expect(await resp!.text(), path).toBe(want)
  }

  // Host-only: the cookie is for that one preview host, and another slug is
  // sent nothing (testing §10.2 item 6's precondition; the item itself needs
  // the proxy).
  stack.previewTap.clear()
  const other = await page.goto(`https://${OTHER_PREVIEW_HOST}/?other=1`)
  const seen = await seenBy(stack.previewTap.seen, (s) => s.path === '/?other=1', 'the other preview host')
  expect(seen.cookieNames).toHaveLength(0)
  expect(other?.status()).toBe(401)
})
