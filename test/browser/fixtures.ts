// Fixtures for the browser tier: one Stack per worker (and the config runs one
// worker), and Playwright's own `context` and `page` on top of the Stack's
// browser — so tracing on failure and per-test isolation of the cookie jar
// come from Playwright rather than from this file.

import { test as base, expect, firefox, webkit, type BrowserContext, type Page } from '@playwright/test'
import { COOKIE, Stack, UI, type Seen } from './harness'

export { expect }

export const test = base.extend<{ signedIn: Page }, { stack: Stack }>({
  stack: [
    // eslint-disable-next-line no-empty-pattern
    async ({}, use) => {
      const s = await Stack.start()
      await use(s)
      await s.teardown()
    },
    { scope: 'worker', timeout: 180_000 },
  ],
  // The Stack's Chromium, which trusts the CA through NSS and resolves the
  // test domains to its Caddy. Playwright's built-in `context` and `page`
  // are created from it.
  //
  // The firefox and webkit projects run only engines.spec.ts, against the
  // Stack's loopback front, so their browser is the plain engine: it needs
  // neither the CA nor the resolver rules, and never ignores a certificate
  // error because it is never shown a certificate.
  browser: [
    async ({ stack, browserName }, use) => {
      if (browserName === 'chromium') return use(stack.browser)
      const b = await (browserName === 'firefox' ? firefox : webkit).launch()
      await use(b)
      await b.close()
    },
    { scope: 'worker' },
  ],
  // Every test starts with an empty tap, so "the server saw nothing" means
  // this test's requests and no one else's.
  page: async ({ page, stack }, use) => {
    stack.apiTap.clear()
    stack.previewTap.clear()
    await use(page)
  },
  /** A page on the UI origin, signed in by the real sign-in route. */
  signedIn: async ({ page, stack }, use) => {
    await page.goto(`${UI}/signin`)
    await signIn(page, stack.password)
    await use(page)
  },
})

/**
 * Signs in with a same-origin fetch from a page already on the UI origin.
 *
 * A bare `fetch()` is mode `cors`, so it sends the page's Origin whatever the
 * app's own client does: it cannot catch a client that sends `Origin: null`,
 * as v0.2.1's did in Safari and Firefox. It is kept because it is the fixture
 * for tests about cookies and cross-site requests, where how the sign-in was
 * made is not the subject. The app's client is covered where it is the
 * subject: ui.spec.ts and engines.spec.ts sign in through the real form and
 * assert the Origin the server received, and engines.spec.ts does it in
 * Chromium, Firefox and WebKit.
 */
export async function signIn(page: Page, password: string): Promise<number> {
  return page.evaluate(async (pw) => {
    const r = await fetch('/api/auth/session', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: pw }),
    })
    return r.status
  }, password)
}

/** The session cookie the browser holds for the UI origin, if any. */
export async function sessionCookie(context: BrowserContext) {
  return (await context.cookies(UI)).find((c) => c.name === COOKIE)
}

/** Waits until the tap has seen a request matching `pred`, and returns it. */
export async function seenBy(seen: Seen[], pred: (s: Seen) => boolean, what: string, ms = 10_000): Promise<Seen> {
  const deadline = Date.now() + ms
  for (;;) {
    const hit = seen.find(pred)
    if (hit && hit.status !== null) return hit
    if (Date.now() > deadline) {
      throw new Error(`the server never saw ${what}; it saw:\n${seen.map((s) => `  ${s.method} ${s.host}${s.path} -> ${s.status}`).join('\n')}`)
    }
    await new Promise((r) => setTimeout(r, 50))
  }
}

/** Creates a secret from the page, through Caddy, the way the UI would. */
export async function putSecret(page: Page, name: string, value = 'v'): Promise<number> {
  return page.evaluate(async ([n, v]) => {
    const r = await fetch(`/api/secrets/${n}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ value: v, reach: 'nothing: a browser-tier fixture', description: '' }),
    })
    return r.status
  }, [name, value] as const)
}
