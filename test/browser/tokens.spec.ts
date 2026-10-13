// Settings → API tokens against the real server, through Caddy
// (vscode-extension §10.0a's "a token minted in Settings reads GET
// /api/meta"): the token the page shows once authenticates as a bearer, alone
// — the request carries no cookie — and once revoked from the same page it is
// a 401.

import { UI } from './harness'
import { expect, seenBy, test } from './fixtures'

/** GET /api/meta from the page with the token as a bearer and no cookie. */
async function metaWith(page: import('@playwright/test').Page, token: string): Promise<{ status: number; body: string }> {
  return page.evaluate(async (t) => {
    const r = await fetch('/api/meta', { credentials: 'omit', headers: { Authorization: `Bearer ${t}` } })
    return { status: r.status, body: await r.text() }
  }, token)
}

test('a token minted in Settings reads GET /api/meta as itself, and is 401 once revoked there', async ({ signedIn: page, stack }) => {
  await page.goto(`${UI}/settings#tokens`)
  const section = page.locator('[data-test=api-tokens]')
  await section.locator('[data-test=token-new]').click()
  await section.locator('[data-test=token-label]').fill('Browser tier')
  await section.locator('[data-test=token-create]').click()
  const value = section.locator('[data-test=token-value]')
  await expect(value).toHaveText(/^drydock_[A-Za-z0-9_-]{43}$/)
  const token = (await value.textContent())!.trim()

  // The row came from the stream, and carries no token.
  const row = section.locator('[data-test=token]', { hasText: 'Browser tier' })
  await expect(row).toHaveCount(1)
  await expect(row).not.toContainText(token)

  stack.apiTap.clear()
  const meta = await metaWith(page, token)
  expect(meta.status).toBe(200)
  const m = JSON.parse(meta.body) as { credential: { kind: string; label: string; scopes: string[] } }
  expect(m.credential).toMatchObject({ kind: 'token', label: 'Browser tier', scopes: ['read', 'lifecycle', 'self'] })
  // At the server: a bearer and no session cookie.
  const seen = await seenBy(stack.apiTap.seen, (s) => s.method === 'GET' && s.path === '/api/meta', 'the bearer GET /api/meta')
  expect(seen.status).toBe(200)
  expect(seen.session).toBeNull()

  // Done hides it; Revoke settles on the event.
  await section.locator('[data-test=token-done]').click()
  await expect(value).toHaveCount(0)
  await row.getByRole('button', { name: 'Revoke' }).click()
  await expect(row.locator('[data-test=token-revoked]')).toBeVisible()
  expect((await metaWith(page, token)).status).toBe(401)
})
