// No iframes in either direction (frontend §8, testing §10.2 item 7): the UI
// sends `frame-ancestors 'none'` so a preview cannot embed the control plane,
// and `frame-src 'none'` so the control plane cannot embed a preview. Both are
// asserted as browser behaviour, not just as header text — a policy the
// browser does not enforce, or one that never reaches it through Caddy, is a
// comment.

import { CSP_EXPECTED, OTHER_PREVIEW_HOST, PREVIEW, PREVIEW_PAGE, UI } from './harness'
import { expect, seenBy, test } from './fixtures'

test('the page carries the security headers through Caddy', async ({ page }) => {
  const resp = await page.goto(`${UI}/signin`)
  expect(resp?.status()).toBe(200)
  const h = resp!.headers()
  expect(h['content-security-policy']).toBe(CSP_EXPECTED)
  expect(h['content-security-policy']).toContain("frame-ancestors 'none'")
  expect(h['content-security-policy']).toContain("frame-src 'none'")
  expect(h['referrer-policy']).toBe('no-referrer')
  expect(h['x-content-type-options']).toBe('nosniff')
  // And the app it guards rendered — the headers did not break the page
  // (no inline script or style that the CSP would refuse).
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()
})

test('the UI cannot be framed from a preview origin', async ({ page, stack }) => {
  await page.goto(PREVIEW_PAGE)
  await page.evaluate(([ui, other]) => {
    for (const [name, src] of [['ui', ui], ['other', other]] as const) {
      const f = document.createElement('iframe')
      f.name = name
      f.src = src
      document.body.append(f)
    }
  }, [`${UI}/signin?framed=1`, `https://${OTHER_PREVIEW_HOST}/.drydock/denied?framed=1`] as const)

  // Control: frames work on this page — a frameable cross-origin document
  // (another preview host's denied page) renders inside it.
  await expect(page.frameLocator('iframe[name=other]').locator('body')).toContainText('This preview is not available')

  // The UI was really requested as a frame and really served: so what keeps
  // it out is the browser enforcing a header, not a request that never
  // happened…
  const req = await seenBy(stack.apiTap.seen, (s) => s.path === '/signin?framed=1', 'the framed request')
  expect(req.secFetchDest).toBe('iframe')
  expect(req.status).toBe(200)
  // …and it did not render: the frame holds Chromium's error page, not the app.
  const ui = page.frame({ name: 'ui' })!
  await expect.poll(() => ui.url()).toMatch(/^chrome-error:/)
  expect(await ui.locator('#signin-h').count()).toBe(0)
  // The header that did it (checked last, so a missing directive fails on
  // the behaviour above rather than on its text).
  expect(String(req.responseHeaders?.['content-security-policy'])).toContain("frame-ancestors 'none'")
})

test('the UI cannot frame a preview', async ({ page, stack }) => {
  // Control: the preview origin answers when navigated to directly.
  const direct = await page.goto(`${PREVIEW_PAGE}?direct=1`)
  expect(direct?.status()).toBe(403) // the real preview socket's denied page: a document, served
  await seenBy(stack.previewTap.seen, (s) => s.path === '/.drydock/denied?direct=1', 'the direct preview request')

  await page.goto(`${UI}/signin`)
  const violation = page.evaluate(
    () =>
      new Promise<string>((resolve) => {
        document.addEventListener('securitypolicyviolation', (e) => resolve(e.effectiveDirective), { once: true })
      }),
  )
  await page.evaluate((src) => {
    const f = document.createElement('iframe')
    f.src = src
    document.body.append(f)
  }, `${PREVIEW}/?framed=1`)
  expect(await violation).toBe('frame-src')
  // Blocked before any request left the browser.
  await page.waitForTimeout(500)
  expect(stack.previewTap.seen.filter((s) => s.path === '/?framed=1')).toHaveLength(0)
})
