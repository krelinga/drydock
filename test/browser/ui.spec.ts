// The UI's auth flow against the real server (testing §10.2 items 10 and 11,
// and §10.3's rule: a server, a cookie jar or a real navigation makes it a
// browser test).

import { randomBytes } from 'node:crypto'
import { DatabaseSync } from 'node:sqlite'
import { UI } from './harness'
import { expect, putSecret, sessionCookie, test } from './fixtures'

async function signInThroughTheForm(page: import('@playwright/test').Page, password: string): Promise<void> {
  await page.getByLabel('Password').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
}

test('signing in through the real UI lands on the home screen', async ({ page, context, stack }) => {
  // Signed out, the app sends the root to sign-in.
  await page.goto(`${UI}/`)
  await expect(page).toHaveURL(`${UI}/signin`)
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()

  // A wrong password is refused, says so, and sets no cookie.
  await signInThroughTheForm(page, 'not-the-password')
  await expect(page.getByRole('alert')).toContainText('That password is not right.')
  expect(await sessionCookie(context)).toBeUndefined()
  await expect(page).toHaveURL(`${UI}/signin`)

  // The right one signs in and lands on the home screen.
  await signInThroughTheForm(page, stack.password)
  await expect(page).toHaveURL(`${UI}/`)
  await expect(page.getByRole('heading', { name: 'Workspaces' })).toBeVisible()
  expect(await sessionCookie(context)).toBeDefined()
})

test('sign-in honours return: a deep link while signed out comes back to where it was going', async ({ page, stack }) => {
  const id = `01BROWSERTIER${randomBytes(4).toString('hex').toUpperCase()}`
  await page.goto(`${UI}/ws/${id}`)
  // Sent to sign-in, carrying the destination…
  await expect(page).toHaveURL((u) => u.pathname === '/signin' && u.searchParams.get('return') === `/ws/${id}`)
  await signInThroughTheForm(page, stack.password)
  // …and brought back to it, not to the home screen (which is where the
  // previous test, with no return, correctly went).
  await expect(page).toHaveURL(`${UI}/ws/${id}`)
})

test('a 401 mid-session routes to sign-in and clears entity state rather than keeping it', async ({ page, stack }) => {
  await page.goto(`${UI}/signin`)
  await signInThroughTheForm(page, stack.password)
  await expect(page.getByRole('heading', { name: 'Workspaces' })).toBeVisible()

  const canary = `STALE_${randomBytes(4).toString('hex').toUpperCase()}`
  expect(await putSecret(page, canary)).toBe(200)
  const nav = page.getByRole('navigation', { name: 'Main' })
  await nav.getByRole('link', { name: 'Secrets' }).click()
  const row = page.locator('[data-test=secret-name]', { hasText: canary })
  await expect(row).toHaveCount(1)

  // From here GET /api/secrets never answers, so the list can only come from
  // entity state the app already holds.
  await page.route(`${UI}/api/secrets`, (r) => r.abort())

  // Control: while the session is alive, held state DOES render through a
  // failed refetch — so its absence after the 401 below is the clearing, not
  // the blocked request.
  await nav.getByRole('link', { name: 'Workspaces' }).click()
  await nav.getByRole('link', { name: 'Secrets' }).click()
  await expect(row).toHaveCount(1)
  await expect(page.locator('[data-test=secrets-stale]')).toBeVisible()

  // The session ends underneath the live page: its row is deleted, the
  // cheapest way to get a 401 the page did not cause (testing §10.2 item 11).
  const db = new DatabaseSync(stack.db)
  try {
    db.exec('PRAGMA busy_timeout = 5000')
    const n = db.prepare('DELETE FROM auth_session').run().changes
    expect(n).toBeGreaterThan(0)
  } finally {
    db.close()
  }

  // The next request the app makes gets a 401, and the app goes to sign-in,
  // carrying where it was.
  await nav.getByRole('link', { name: 'Workspaces' }).click()
  await expect(page).toHaveURL(/\/signin(\?|$)/)
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()
  await expect(page.getByText(canary)).toHaveCount(0)

  // Sign in again and go back to the list. If the entities had been kept,
  // the canary would render exactly as it did in the control above.
  await signInThroughTheForm(page, stack.password)
  await expect(page).not.toHaveURL(/\/signin/)
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Secrets' }).click()
  await expect(row).toHaveCount(0)
  await expect(page.locator('[data-test=secrets-error]')).toBeVisible()
})
