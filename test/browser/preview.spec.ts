// Previews in a browser (port forwarding §7, §13 steps 1–2; testing §10.2
// items 5 and 6, §10.4): the real `drydock serve` and real Caddy, Chromium
// trusting the tier's CA.
//
// Step 2's handshake: a signed-in device clicks through to a preview host and
// lands on the hardcoded upstream (internal/preview.Placeholder) after three
// redirects — preview host → /preview/authorize on the UI → the preview host's
// /.drydock/session?t=… → the clean URL — holding only the host-only preview
// cookie there; a device that is not signed in is bounced to sign-in and
// brought back; and Sign out everywhere closes the preview on its next
// request. The token is in no file the stack wrote.
//
// Assertions about cookies are made at the server, through the taps, as in
// crosssite.spec.ts.

import { readdirSync, readFileSync, statSync } from 'node:fs'
import path from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import {
  COOKIE,
  OTHER_PREVIEW_HOST,
  PREVIEW,
  PREVIEW_COOKIE,
  PREVIEW_DOMAIN,
  PREVIEW_HOST,
  SLUG,
  SLUG_HOST,
  UI,
  type Stack,
} from './harness'
import { expect, seenBy, sessionCookie, test } from './fixtures'

const TARGET = `https://${SLUG_HOST}`

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

/** What PF §13 step 4's registry will write: one enabled port on a running workspace. */
function seed(stack: Stack): void {
  const db = new DatabaseSync(stack.db)
  try {
    db.exec('PRAGMA busy_timeout = 5000')
    db.exec(`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (424242, 1, 'o/myapp', 'main')`)
    db.exec(`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('wpreview', 424242, '/x', 'main', 'running')`)
    db.exec(`INSERT INTO forwarded_port (id, workspace_id, container_port, slug, enabled) VALUES ('ppreview', 'wpreview', 5173, '${SLUG}', 1)`)
  } finally {
    db.close()
  }
}

function unseed(stack: Stack): void {
  const db = new DatabaseSync(stack.db)
  try {
    db.exec('PRAGMA busy_timeout = 5000')
    db.exec(`DELETE FROM preview_session WHERE forwarded_port_id = 'ppreview'`)
    db.exec(`DELETE FROM forwarded_port WHERE id = 'ppreview'`)
    db.exec(`DELETE FROM workspace WHERE id = 'wpreview'`)
    db.exec(`DELETE FROM repository WHERE id = 424242`)
  } finally {
    db.close()
  }
}

/** Every regular file under dir that contains needle. */
function filesContaining(dir: string, needle: string): string[] {
  const out: string[] = []
  const walk = (d: string) => {
    for (const name of readdirSync(d)) {
      const p = path.join(d, name)
      const st = statSync(p, { throwIfNoEntry: false })
      if (!st) continue
      if (st.isDirectory()) walk(p)
      else if (st.isFile() && readFileSync(p).includes(needle)) out.push(p)
    }
  }
  walk(dir)
  return out
}

test.describe('the handshake', () => {
  test.beforeEach(({ stack }) => seed(stack))
  test.afterEach(({ stack }) => unseed(stack))

  test('a signed-in device clicks through to a preview and lands on it, holding only the preview cookie there', async ({
    signedIn: page,
    context,
    stack,
  }) => {
    const session = (await sessionCookie(context))!.value
    // The app's own cookie on the preview host: it must pass to the upstream
    // (the control for the strip).
    await context.addCookies([{ name: 'app-own', value: 'kept', url: `${TARGET}/`, secure: true }])
    stack.previewTap.clear()
    stack.apiTap.clear()

    const landing = `${TARGET}/app/page?x=1`
    await clickTo(page, landing)
    await expect(page.locator('[data-test=placeholder]')).toBeVisible()
    // The landing URL is clean: the token URL is never the app's address.
    expect(page.url()).toBe(landing)

    // 1 → 2: the preview socket, no preview cookie and no UI cookie (it is
    // cross-site and host-only), to authorize on the UI origin.
    const first = await seenBy(stack.previewTap.seen, (s) => s.host === SLUG_HOST && s.path === '/app/page?x=1', 'the first preview request')
    expect(first.secFetchSite).toBe('cross-site')
    expect(first.session).toBeNull()
    expect(first.cookieNames).not.toContain(PREVIEW_COOKIE)
    expect(first.status).toBe(302)
    expect(first.responseHeaders!.location).toBe(`${UI}/preview/authorize?return=${encodeURIComponent(landing)}`)
    expect(first.responseHeaders!['referrer-policy']).toBe('no-referrer')

    // 3 → 5: the session cookie rides the cross-site top-level redirect
    // (SameSite=Lax), and authorize mints.
    const auth = await seenBy(stack.apiTap.seen, (s) => s.path.startsWith('/preview/authorize?'), 'authorize')
    expect(auth.session).toBe(session)
    expect(auth.secFetchSite).toBe('cross-site')
    expect(auth.status).toBe(302)
    const tokenURL = new URL(String(auth.responseHeaders!.location))
    expect(tokenURL.host).toBe(SLUG_HOST)
    expect(tokenURL.pathname).toBe('/.drydock/session')
    const token = tokenURL.searchParams.get('t')!
    expect(token.length).toBeGreaterThan(20)

    // 6: consumed, the host-only cookie set, the clean path.
    const consume = await seenBy(stack.previewTap.seen, (s) => s.path.startsWith('/.drydock/session?'), 'the token')
    expect(consume.status).toBe(302)
    expect(consume.session).toBeNull()
    const h = consume.responseHeaders!
    expect(h['referrer-policy']).toBe('no-referrer')
    expect(h['cache-control']).toBe('no-store')
    expect(h.location).toBe(landing)
    const setCookie = String(h['set-cookie'])
    expect(setCookie).toMatch(new RegExp(`^${PREVIEW_COOKIE}=`))
    for (const attr of ['Path=/', 'HttpOnly', 'Secure', 'SameSite=Lax']) expect(setCookie).toContain(attr)
    expect(setCookie).not.toMatch(/Domain=/i)

    // The landing request: the preview cookie arrived at Drydock (which
    // strips it before the upstream), no Referer carries the token, and the
    // upstream saw the app's cookie and not the preview cookie.
    const last = stack.previewTap.seen.filter((s) => s.path === '/app/page?x=1').at(-1)!
    expect(last.status).toBe(200)
    expect(last.cookieNames).toContain(PREVIEW_COOKIE)
    expect(last.session).toBeNull()
    expect(last.referer ?? '').not.toContain(token)
    await expect(page.locator('[data-test=app-cookies]')).toHaveText('app-own')

    // The jar: on the preview host, exactly the preview cookie (and the
    // app's), host-only; on another preview host and the UI, no preview
    // cookie at all.
    const jar = await context.cookies(`${TARGET}/`)
    const pc = jar.find((c) => c.name === PREVIEW_COOKIE)!
    expect(jar.map((c) => c.name).sort()).toEqual([PREVIEW_COOKIE, 'app-own'].sort())
    expect(pc.domain).toBe(SLUG_HOST) // no leading dot: host-only
    expect(pc.httpOnly && pc.secure && pc.sameSite === 'Lax').toBe(true)
    expect((await context.cookies(`https://${OTHER_PREVIEW_HOST}/`)).map((c) => c.name)).not.toContain(PREVIEW_COOKIE)
    expect((await context.cookies(`${UI}/`)).map((c) => c.name)).toEqual([COOKIE])

    // Spent: the token URL again is the dead end.
    const again = await page.goto(tokenURL.toString())
    expect(again?.status()).toBe(403)
    expect(page.url()).toBe(`${TARGET}/.drydock/denied`)

    // The token and the cookie are in nothing the stack wrote: Caddy's
    // output, Drydock's log, the database (which holds the cookie's hash).
    expect(filesContaining(stack.root, token), 'the token').toEqual([])
    expect(filesContaining(stack.root, pc.value), 'the preview cookie').toEqual([])
  })

  test('a device that is not signed in is sent to sign-in, and brought back to the preview', async ({ page, stack }) => {
    const landing = `${TARGET}/hello?y=2`
    await page.goto(landing)
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    const at = new URL(page.url())
    expect(at.origin).toBe(UI)
    expect(at.pathname).toBe('/signin')
    expect(at.searchParams.get('return')).toBe(`/preview/authorize?return=${encodeURIComponent(landing)}`)

    await page.getByLabel('Password').fill(stack.password)
    await page.getByRole('button', { name: 'Sign in' }).click()
    await expect(page.locator('[data-test=placeholder]')).toBeVisible()
    expect(page.url()).toBe(landing)
  })

  test('Sign out everywhere closes an open preview on its next request', async ({ signedIn: page, context, stack }) => {
    await clickTo(page, `${TARGET}/`)
    await expect(page.locator('[data-test=placeholder]')).toBeVisible()
    // Control: the next request is served, without a handshake.
    stack.previewTap.clear()
    await page.reload()
    await expect(page.locator('[data-test=placeholder]')).toBeVisible()
    expect(stack.previewTap.seen.map((s) => s.status)).toEqual([200])

    // The Settings page's own button.
    await page.goto(`${UI}/settings`)
    await page.locator('[data-test=sign-out-everywhere]').click()
    await page.locator('[data-test=confirm-everywhere] [data-test=confirm]').click()
    await expect(page).toHaveURL(/\/signin/)

    // The preview cookie is still in the jar, and arrives — and is refused:
    // the device is sent through the handshake and on to sign-in.
    expect((await context.cookies(`${TARGET}/`)).map((c) => c.name)).toContain(PREVIEW_COOKIE)
    stack.previewTap.clear()
    await page.goto(`${TARGET}/after`)
    const seen = await seenBy(stack.previewTap.seen, (s) => s.path === '/after', 'the preview after revoke-all')
    expect(seen.cookieNames).toContain(PREVIEW_COOKIE)
    expect(seen.status).toBe(302)
    expect(String(seen.responseHeaders!.location)).toContain('/preview/authorize?')
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()
  })
})

test('an unknown slug and the installer probe land on the denied page, which says nothing', async ({ signedIn: page, stack }) => {
  // A well-formed slug with no port behind it: the handshake runs, and
  // authorize — signed in — sends it to its own dead end.
  await clickTo(page, `${PREVIEW}/x`)
  await expect(page.getByRole('heading', { name: 'This preview is not available' })).toBeVisible()
  expect(page.url()).toBe(`${PREVIEW}/.drydock/denied`)
  const denied = await seenBy(stack.previewTap.seen, (s) => s.path === '/.drydock/denied', 'the denied page')
  expect(denied.status).toBe(403)
  const body = await page.content()
  for (const leak of ['abc123', '5173', UI, 'workspace stopped', 'not found']) expect(body).not.toContain(leak)

  // The probe name is never a slug: denied at once, no handshake.
  stack.previewTap.clear()
  stack.apiTap.clear()
  const resp = await page.goto(`https://drydock-check.${PREVIEW_DOMAIN}/`)
  expect(resp?.status()).toBe(403)
  expect(stack.apiTap.seen.filter((s) => s.path.startsWith('/preview/authorize'))).toHaveLength(0)
  const probe = await seenBy(stack.previewTap.seen, (s) => s.path === '/', 'the probe')
  expect(probe.status).toBe(302)
  expect(probe.responseHeaders!.location).toBe('/.drydock/denied')
})

test('a forged preview cookie and a forged token reach the preview socket through Caddy, and change nothing', async ({
  page,
  context,
  stack,
}) => {
  // Without a cookie: the handshake's redirect.
  await page.goto(`${PREVIEW}/?bare=1`).catch(() => {})
  const bare = await seenBy(stack.previewTap.seen, (s) => s.path === '/?bare=1', 'the bare request')

  // A forged host-only preview cookie, in the shape the handshake sets.
  await context.addCookies([{ name: PREVIEW_COOKIE, value: 'forged', url: `${PREVIEW}/`, secure: true, httpOnly: true, sameSite: 'Lax' }])
  await page.goto(`${PREVIEW}/?forged=1`).catch(() => {})
  const forged = await seenBy(stack.previewTap.seen, (s) => s.path === '/?forged=1', 'the forged cookie')
  // It arrived (Caddy's hop passes it, PF §9), and was refused exactly as
  // no cookie was: the same redirect, but for the URL itself.
  expect(forged.cookieNames).toContain(PREVIEW_COOKIE)
  expect(bare.cookieNames).not.toContain(PREVIEW_COOKIE)
  expect(forged.status).toBe(bare.status)
  expect(String(forged.responseHeaders!.location).replace('forged', 'bare')).toBe(String(bare.responseHeaders!.location))

  // A forged token, and none: the same dead end, with no-referrer and no cookie set.
  for (const p of ['/.drydock/session?t=forged', '/.drydock/session']) {
    stack.previewTap.clear()
    await page.goto(`${PREVIEW}${p}`)
    const s = await seenBy(stack.previewTap.seen, (x) => x.path === p, p)
    expect(s.status, p).toBe(302)
    expect(s.responseHeaders!.location, p).toBe('/.drydock/denied')
    expect(s.responseHeaders!['referrer-policy'], p).toBe('no-referrer')
    expect(s.responseHeaders!['cache-control'], p).toBe('no-store')
    expect(s.responseHeaders!['set-cookie'], p).toBeUndefined()
  }

  // Host-only: the forged cookie is for that one preview host, and another
  // slug is sent nothing (testing §10.2 item 6's precondition).
  stack.previewTap.clear()
  await page.goto(`https://${OTHER_PREVIEW_HOST}/.drydock/denied?other=1`)
  const other = await seenBy(stack.previewTap.seen, (s) => s.path === '/.drydock/denied?other=1', 'the other preview host')
  expect(other.cookieNames).toHaveLength(0)
  expect(PREVIEW_HOST).not.toBe(OTHER_PREVIEW_HOST)
})
