// The cross-site boundary, end to end (testing §10.2 items 1–3 and 5; port
// forwarding §14.1; Spike 04 D0–D2, E1, F1).
//
// `drydock.test` and `*.drydock-preview.test` are distinct registrable domains
// (`.test` is not on the Public Suffix List), which is the property PF §4 buys
// with a second real domain. The "attacker" is a document on the preview
// origin, served by the real Drydock preview socket through the real Caddy.
//
// Every assertion about a cookie is made at the server, through the tap: a
// cross-site fetch that was blocked and one that was sent without credentials
// look the same from inside the page.

import type { BrowserContext, Page } from '@playwright/test'
import { COOKIE, PREVIEW, PREVIEW_HOST, PREVIEW_PAGE, UI, UI_HOST } from './harness'
import { expect, seenBy, sessionCookie, test } from './fixtures'

const site = (host: string) => host.split('.').slice(-2).join('.')

async function onPreview(page: Page): Promise<void> {
  await page.goto(PREVIEW_PAGE)
  expect(new URL(page.url()).host).toBe(PREVIEW_HOST)
}

test('SameSite=Lax: a cross-site POST and GET carry no cookie, a cross-site top-level navigation does', async ({ signedIn: page, context, stack }) => {
  const cookie = (await sessionCookie(context))!.value

  // Control for the whole test: the jar has the cookie, and a same-site
  // request carries it.
  stack.apiTap.clear()
  await page.evaluate(async () => {
    await fetch('/api/secrets')
  })
  expect((await seenBy(stack.apiTap.seen, (s) => s.path === '/api/secrets', 'the same-site GET')).session).toBe(cookie)

  await onPreview(page)
  // D0: genuinely a different site, by the browser's own reckoning.
  expect(site(PREVIEW_HOST)).not.toBe(site(UI_HOST))
  stack.apiTap.clear()

  // A credentialed cross-site POST at a mutating route. `no-cors` is what an
  // attacker would use: the response is opaque but the request is sent.
  await page.evaluate(async (u) => {
    await fetch(u, { method: 'POST', mode: 'no-cors', credentials: 'include' }).catch(() => {})
  }, `${UI}/api/repos/refresh`)
  // A credentialed cross-site GET of a data route.
  await page.evaluate(async (u) => {
    await fetch(u, { mode: 'no-cors', credentials: 'include' }).catch(() => {})
  }, `${UI}/api/secrets`)

  // D2, the control for D1: the POST arrived — so "no cookie" is not "no request".
  const post = await seenBy(stack.apiTap.seen, (s) => s.method === 'POST' && s.path === '/api/repos/refresh', 'the cross-site POST')
  expect(post.secFetchSite).toBe('cross-site')
  // D1: no cookie, and refused before any handler ran.
  expect(post.session).toBeNull()
  expect([401, 403]).toContain(post.status)
  // F1: the GET, likewise.
  const get = await seenBy(stack.apiTap.seen, (s) => s.method === 'GET' && s.path === '/api/secrets', 'the cross-site GET')
  expect(get.secFetchSite).toBe('cross-site')
  expect(get.session).toBeNull()
  expect(get.status).toBe(401)

  // E1: a cross-site *top-level navigation* — the preview authorize hop —
  // does carry it. This is the measured reason the cookie is Lax and not
  // Strict. It must be a real click: page.goto is browser-initiated
  // (sec-fetch-site: none) and carries the cookie under any SameSite value,
  // so a goto here would pass against Strict too.
  await page.evaluate((u) => {
    const a = document.createElement('a')
    a.href = u
    a.id = 'go'
    a.textContent = 'authorize'
    document.body.append(a)
  }, `${UI}/preview/authorize?probe=1`)
  await page.click('#go')
  const nav = await seenBy(stack.apiTap.seen, (s) => s.path === '/preview/authorize?probe=1', 'the cross-site navigation')
  expect(nav.secFetchSite).toBe('cross-site')
  expect(nav.secFetchMode).toBe('navigate')
  expect(nav.session).toBe(cookie)
})

/**
 * Makes the browser behave as if SameSite had failed: the jar's session cookie
 * is replaced by the same value marked SameSite=None, so a cross-site request
 * really does carry it. (A route cannot add a Cookie header — Chromium
 * applies the jar regardless — so the jar is what has to change.) What is
 * under test is then Drydock's half of the belt, on a request the browser
 * really sent.
 */
async function sameSiteFailed(context: BrowserContext, cookie: string): Promise<void> {
  await context.addCookies([{ name: COOKIE, value: cookie, url: `${UI}/`, secure: true, httpOnly: true, sameSite: 'None' }])
}

test('the Origin check refuses a cross-site POST even when SameSite lets the cookie through', async ({ signedIn: page, context, stack }) => {
  const cookie = (await sessionCookie(context))!.value
  const post = (tag: string, opts: { method: 'POST'; mode?: RequestMode; credentials?: RequestCredentials }) =>
    page.evaluate(async ([u, o]) => {
      await fetch(u, o).catch(() => {})
    }, [`${UI}/api/repos/refresh?${tag}`, opts] as const)
  const seen = (tag: string) =>
    seenBy(stack.apiTap.seen, (s) => s.path === `/api/repos/refresh?${tag}`, `the ${tag} POST`)

  // The positive control: a same-origin POST, which the browser sends with
  // the cookie and Origin: https://drydock.test, gets past the gate — the
  // handler answers (there is no GitHub App here, so it says so).
  stack.apiTap.clear()
  await post('control', { method: 'POST' })
  const ok = await seen('control')
  expect(ok.session).toBe(cookie)
  expect(ok.origin).toBe(UI)
  expect([401, 403]).not.toContain(ok.status)

  await sameSiteFailed(context, cookie)
  await onPreview(page)
  stack.apiTap.clear()
  // The attacking page's own Origin, and the opaque "null" a sandboxed
  // frame sends. A browser cannot be made to send a cross-site POST with no
  // Origin at all; that case is the component tier's (testing §8.1, "Origin
  // is exact-match").
  await post('preview', { method: 'POST', mode: 'no-cors', credentials: 'include' })
  await page.evaluate(() => {
    const f = document.createElement('iframe')
    f.name = 'opaque'
    f.sandbox.add('allow-scripts')
    f.srcdoc = '<p>an opaque origin</p>'
    document.body.append(f)
  })
  await expect.poll(() => page.frame({ name: 'opaque' })?.url()).toBe('about:srcdoc')
  await page.frame({ name: 'opaque' })!.evaluate(async (u) => {
    await fetch(u, { method: 'POST', mode: 'no-cors', credentials: 'include' }).catch(() => {})
  }, `${UI}/api/repos/refresh?null`)
  for (const [tag, origin] of [['preview', PREVIEW], ['null', 'null']] as const) {
    const s = await seen(tag)
    expect(s.session, `${tag}: the cookie must arrive, or this proves nothing`).toBe(cookie)
    expect(s.origin, tag).toBe(origin)
    expect(s.status, tag).toBe(403)
  }
})

test('a data GET cannot be read cross-origin, even with the cookie', async ({ signedIn: page, context, stack }) => {
  const cookie = (await sessionCookie(context))!.value

  // Control: same-origin, the body is readable.
  const same = await page.evaluate(async () => {
    const r = await fetch('/api/auth/session')
    return { status: r.status, body: await r.text() }
  })
  expect(same.status).toBe(200)
  expect(same.body).toContain('"devices"')

  // Let the cookie through, so the server really does answer 200 with data:
  // the only thing left between that data and the attacking page is CORS.
  await sameSiteFailed(context, cookie)
  await onPreview(page)
  stack.apiTap.clear()
  const cross = await page.evaluate(async (u) => {
    try {
      const r = await fetch(u, { credentials: 'include' })
      return { read: true, body: await r.text() }
    } catch (e) {
      return { read: false, body: String(e) }
    }
  }, `${UI}/api/auth/session`)

  const seen = await seenBy(stack.apiTap.seen, (s) => s.path === '/api/auth/session', 'the cross-origin GET')
  expect(seen.session).toBe(cookie)
  expect(seen.status).toBe(200) // the server did hand over the data…
  expect(seen.responseHeaders?.['access-control-allow-origin']).toBeUndefined()
  expect(cross.read).toBe(false) // …and the browser kept it from the page
  expect(cross.body).toContain('TypeError')
})
