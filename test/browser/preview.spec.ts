// Previews in a browser (port forwarding §7, §8, §13 steps 1–3; testing §10.2
// items 5 and 6, §10.4): the real `drydock serve` and real Caddy, Chromium
// trusting the tier's CA.
//
// Step 2's handshake: a signed-in device clicks through to a preview host and
// lands on the app after three redirects — preview host → /preview/authorize
// on the UI → the preview host's /.drydock/session?t=… → the clean URL —
// holding only the host-only preview cookie there; a device that is not signed
// in is bounced to sign-in and brought back; and Sign out everywhere closes
// the preview on its next request. The token is in no file the stack wrote.
//
// Step 3's proxy: the app is a real Vite dev server, reached through the
// proxy at the address the stand-in docker reports for the seeded workspace's
// container (harness.ts) — this host's own non-loopback address, since Drydock
// refuses a loopback one. The page loads, Vite's HMR websocket connects
// through Caddy and the proxy, a custom event goes up it and comes back, and
// an edit to the module on disk updates the page in place. With the container
// gone, the next request is the denied page.
//
// Assertions about cookies are made at the server, through the taps, as in
// crosssite.spec.ts.

import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
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
  DevServer,
  hostAddress,
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

/** The seeded workspace: a ULID, since the container is found by its label. */
const WS = '01JPREV0000000000000000000'

/**
 * The previewed app, a Vite project: a page whose module accepts its own hot
 * updates and echoes a custom event over the HMR socket, and a plugin that
 * answers the handshake tests' landing paths with the names — never the
 * values — of the cookies the app was sent.
 */
const MAIN = (version: string) => `document.getElementById('out').textContent = '${version}'
if (import.meta.hot) {
  import.meta.hot.accept()
  import.meta.hot.on('drydock:echo', (d) => { document.getElementById('echo').textContent = d.n })
  import.meta.hot.send('drydock:echo', { n: 'round trip' })
}
`
const APP = {
  'index.html': `<!doctype html><html><head><meta charset="utf-8"><title>vite app</title></head>
<body><p id="out">loading</p><p id="echo">no echo</p><script type="module" src="/main.js"></script></body></html>`,
  'main.js': MAIN('version one'),
  'vite.config.mjs': `export default {
  logLevel: 'warn',
  plugins: [{
    name: 'drydock-browser-tier',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const p = new URL(req.url, 'http://x').pathname
        if (!(p.startsWith('/app/') || p === '/hello' || p === '/after')) return next()
        const names = (req.headers.cookie ?? '').split(';').map((c) => c.split('=')[0].trim()).filter(Boolean).sort()
        res.setHeader('Content-Type', 'text/html; charset=utf-8')
        res.end('<!doctype html><meta charset="utf-8"><title>app</title><p data-test="upstream">the app</p>' +
          '<p>Cookies: <span data-test="app-cookies">' + (names.join(', ').replace(/[<>&]/g, '') || 'none') + '</span></p>')
      })
      server.ws.on('drydock:echo', (data, client) => client.send('drydock:echo', data))
    },
  }],
}
`,
}

let dev: DevServer

test.beforeAll(async ({ stack }) => {
  dev = await DevServer.start(path.join(stack.root, 'vite-app'), APP)
})
test.afterAll(async () => {
  await dev?.stop()
})

/** One enabled port on a running workspace, whose container runs: what the registry's enable leaves. */
function seed(stack: Stack): void {
  const db = new DatabaseSync(stack.db)
  try {
    db.exec('PRAGMA busy_timeout = 5000')
    db.exec(`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (424242, 1, 'o/myapp', 'main')`)
    db.exec(`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('${WS}', 424242, '/x', 'main', 'running')`)
    db.exec(`INSERT INTO forwarded_port (id, workspace_id, container_port, slug, enabled) VALUES ('ppreview', '${WS}', ${dev.port}, '${SLUG}', 1)`)
  } finally {
    db.close()
  }
  stack.previewContainer(WS, hostAddress())
}

function unseed(stack: Stack): void {
  stack.previewContainer(WS, null)
  const db = new DatabaseSync(stack.db)
  try {
    db.exec('PRAGMA busy_timeout = 5000')
    db.exec(`DELETE FROM preview_session WHERE forwarded_port_id = 'ppreview'`)
    db.exec(`DELETE FROM forwarded_port WHERE id = 'ppreview'`)
    db.exec(`DELETE FROM workspace WHERE id = '${WS}'`)
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
    await expect(page.locator('[data-test=upstream]')).toBeVisible()
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
    await expect(page.locator('[data-test=upstream]')).toBeVisible()
    expect(page.url()).toBe(landing)
  })

  test('Sign out everywhere closes an open preview on its next request', async ({ signedIn: page, context, stack }) => {
    await clickTo(page, `${TARGET}/app/`)
    await expect(page.locator('[data-test=upstream]')).toBeVisible()
    // Control: the next request is served, without a handshake.
    stack.previewTap.clear()
    await page.reload()
    await expect(page.locator('[data-test=upstream]')).toBeVisible()
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

  test('Vite with HMR through Caddy and the proxy: the page loads, its websocket carries both ways, and an edit updates it in place', async ({
    signedIn: page,
    stack,
  }) => {
    await clickTo(page, `${TARGET}/`)
    await expect(page.locator('#out')).toHaveText('version one')
    // Up and back down the HMR socket: the module sends a custom event and
    // the plugin answers it to the same client.
    await expect(page.locator('#echo')).toHaveText('round trip')
    const ws = await seenBy(stack.previewTap.seen, (s) => s.upgrade === 'websocket', 'the HMR websocket')
    expect(ws.status).toBe(101)
    expect(ws.host).toBe(SLUG_HOST)
    expect(ws.cookieNames).toContain(PREVIEW_COOKIE)
    expect(ws.responseHeaders!['set-cookie'] ?? []).toEqual([])

    // A hot update, not a reload: the marker survives.
    await page.evaluate(() => ((window as unknown as { marker: number }).marker = 42))
    writeFileSync(path.join(dev.root, 'main.js'), MAIN('version two'))
    await expect(page.locator('#out')).toHaveText('version two')
    expect(await page.evaluate(() => (window as unknown as { marker?: number }).marker)).toBe(42)

    // The container gone behind Drydock's back — its dev server with it, and
    // Docker reporting nothing running — while the row still says running:
    // the next request is the dead end, never a dial to the address it had
    // (PF §8.1).
    stack.previewContainer(WS, null)
    await dev.stop()
    const resp = await page.goto(`${TARGET}/`)
    expect(resp?.status()).toBe(403)
    expect(page.url()).toBe(`${TARGET}/.drydock/denied`)
    // And back: resolved again, served again.
    await dev.restart()
    stack.previewContainer(WS, hostAddress())
    await page.goto(`${TARGET}/`)
    await expect(page.locator('#out')).toHaveText('version two')
  })
})

/** A port listed by hand and switched off, as POST …/ports leaves it: what the panel enables. */
function seedOff(stack: Stack): void {
  const db = new DatabaseSync(stack.db)
  try {
    db.exec('PRAGMA busy_timeout = 5000')
    db.exec(`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (424242, 1, 'o/myapp', 'main')`)
    db.exec(`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('${WS}', 424242, '/x', 'main', 'running')`)
    db.exec(`INSERT INTO forwarded_port (id, workspace_id, container_port, slug, label, manual) VALUES ('ppreview', '${WS}', ${dev.port}, '${SLUG}', 'vite', 1)`)
  } finally {
    db.close()
  }
  stack.previewContainer(WS, hostAddress())
}

// PF §13 step 4's done-when, through real Caddy: enable a port from the
// workspace's panel, open it, disable it, and watch it close — the open HMR
// websocket at once (the recheck is 30 s away), and the next load of the
// preview ending on its own host with Clear-Site-Data (PF §10.3), which the
// browser honours: what the page stored there is gone.
test.describe('the ports panel', () => {
  test.beforeEach(({ stack }) => seedOff(stack))
  test.afterEach(({ stack }) => unseed(stack))

  test('enable, open, disable: the preview closes', async ({ signedIn: page, context, stack }) => {
    if (dev.proc === null) await dev.restart()
    writeFileSync(path.join(dev.root, 'main.js'), MAIN('version one')) // the HMR test above left it edited
    await page.goto(`${UI}/ws/${WS}`)
    const row = page.locator(`[data-test=port][data-port="${dev.port}"]`)
    await expect(row.locator('[data-test=port-off]')).toBeVisible()
    // Off: the preview host is refused before anything is enabled (control).
    await expect(row.locator('[data-test=port-open]')).toHaveCount(0)

    await row.locator('[data-test=port-enable] [data-test=action]').click()
    const link = row.locator('[data-test=port-open]')
    await expect(link).toHaveText(SLUG_HOST)
    expect(await link.getAttribute('target')).toBe('_blank')
    expect(await link.getAttribute('rel')).toBe('noopener noreferrer')
    const patch = await seenBy(stack.apiTap.seen, (s) => s.method === 'PATCH' && s.path === `/api/workspaces/${WS}/ports/ppreview`, 'the enable')
    expect(patch.status).toBe(202)

    // Open it: a new tab, through the handshake, onto the app and its HMR socket.
    const [preview] = await Promise.all([context.waitForEvent('page'), link.click()])
    const closed = new Promise<void>((resolve) => {
      preview.on('websocket', (ws) => ws.on('close', () => resolve()))
    })
    await expect(preview.locator('#out')).toHaveText('version one')
    await expect(preview.locator('#echo')).toHaveText('round trip')
    expect(new URL(preview.url()).host).toBe(SLUG_HOST)
    await preview.evaluate(() => localStorage.setItem('left-behind', 'by the app'))
    expect(await preview.evaluate(() => localStorage.getItem('left-behind'))).toBe('by the app')

    // Disable it: the link goes, and the websocket closes at once.
    const disabledAt = Date.now()
    await row.locator('[data-test=port-disable] [data-test=action]').click()
    await expect(row.locator('[data-test=port-open]')).toHaveCount(0)
    await expect(row.locator('[data-test=port-enable] [data-test=action]')).toBeEnabled()
    await closed
    expect(Date.now() - disabledAt).toBeLessThan(15_000)

    // Another preview's session, held on another host of the same preview
    // domain: clearing this one's site must not touch it (PF §10.3, §10.4).
    await context.addCookies([{ name: PREVIEW_COOKIE, value: 'other-preview', url: `https://${OTHER_PREVIEW_HOST}/`, secure: true, httpOnly: true, sameSite: 'Lax' }])

    // The next load: the handshake, then the host's own dead end, clearing it.
    stack.previewTap.clear()
    await preview.goto(`${TARGET}/`).catch(() => {}) // Vite's client may be reloading it already
    await expect(preview.getByRole('heading', { name: 'This preview is not available' })).toBeVisible()
    const cleared = await seenBy(stack.previewTap.seen, (s) => s.path.startsWith('/.drydock/session?'), 'the clearing landing')
    expect(cleared.status).toBe(403)
    expect(cleared.responseHeaders!['clear-site-data']).toBe('"cache", "storage"')
    expect(cleared.responseHeaders!['set-cookie']).toBeUndefined()
    // This origin's storage is gone…
    expect(await preview.evaluate(() => localStorage.getItem('left-behind'))).toBeNull()
    // …and the other preview's cookie is not: "cookies" would have cleared
    // the whole registrable domain.
    const other = (await context.cookies(`https://${OTHER_PREVIEW_HOST}/`)).find((c) => c.name === PREVIEW_COOKIE)
    expect(other?.value).toBe('other-preview')
    await preview.close()
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
