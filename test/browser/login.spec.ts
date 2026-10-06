// The Claude login handshake through the real stack (testing §10.2 item 12):
// Chromium, Caddy on the shipped Caddyfile, the real `drydock serve`, its
// login manager on a real PTY — with fakeclaude behind the harness's
// stand-in docker where the login container's claude would be.
//
// Item 12 is the reason the handshake's state lives on the server: the
// operator leaves for their browser to authorize, and the page may be
// discarded meanwhile (frontend §2.4). A reload is a real navigation, so only
// this tier reaches it. The second half is the code's path: it goes up in a
// request body, is typed into the PTY, and is in nothing the stack writes or
// any URL a request carried.

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { createHash } from 'node:crypto'
import path from 'node:path'
import { UI } from './harness'
import { expect, test } from './fixtures'

/** Every regular file under dir, recursively. */
function files(dir: string): string[] {
  const out: string[] = []
  for (const name of readdirSync(dir)) {
    const p = path.join(dir, name)
    const st = statSync(p, { throwIfNoEntry: false })
    if (st?.isDirectory()) out.push(...files(p))
    else if (st?.isFile()) out.push(p)
  }
  return out
}

test('the login handshake survives a reload, and the code reaches the PTY and nothing else', async ({ signedIn: page, stack }) => {
  await page.goto(`${UI}/settings#claude`)
  const section = page.locator('[data-test="claude-identity"]')
  await expect(section.locator('[data-test="claude-state"]')).toHaveText('No one has signed in yet.')

  // On this page the handshake's button is the one Sign in to Claude: the
  // banner says what is wrong and offers no second.
  await expect(page.getByRole('link', { name: 'Sign in to Claude' })).toHaveCount(0)
  await section.getByRole('button', { name: 'Sign in to Claude' }).click()

  const link = section.locator('[data-test="login-url"]')
  await expect(link).toBeVisible({ timeout: 30_000 })
  const url = await link.getAttribute('href')
  expect(url).toMatch(/^https:\/\/claude\.com\/cai\/oauth\/authorize\?/)
  expect(new URL(url!).searchParams.get('state')).toBeTruthy()
  await expect(section.locator('[data-test="login-deadline"]')).toContainText(/Time left: [45]:\d\d/)

  // The app switch: the page is gone and comes back. Same login, same link,
  // the countdown still running — read back from the server.
  await page.reload()
  await expect(section.locator('[data-test="login-url"]')).toHaveAttribute('href', url!)
  await expect(section.locator('[data-test="login-deadline"]')).toContainText(/Time left: [45]:\d\d/)

  stack.apiTap.clear()
  await section.locator('[data-test="login-code"]').fill(stack.loginCode)
  await section.getByRole('button', { name: 'Submit code' }).click()
  // (That the field is cleared before the request, not on the verdict, is
  // the frontend tier's: here the verdict can land before a look at it.)
  await expect(section.locator('[data-test="login-succeeded"]')).toBeVisible({ timeout: 30_000 })
  // The watch was told, and found the login the handshake made.
  await expect(section.locator('[data-test="claude-state"]')).not.toHaveText('No one has signed in yet.', { timeout: 30_000 })
  await expect(section.locator('[data-test="claude-account"]')).toHaveText('fixture@example.invalid')

  // The code's request: a POST with the UI's Origin, and the code in its
  // body — never in its path.
  const posts = stack.apiTap.seen.filter((s) => s.method === 'POST' && /\/api\/auth\/claude\/login\/[0-9a-f]{24}\/code$/.test(s.path))
  expect(posts.length).toBe(1)
  expect(posts[0]!.origin).toBe(UI)
  expect(posts[0]!.status).toBe(202)

  // Control: the code really reached the PTY — fakeclaude logs its hash.
  const evs = readFileSync(path.join(stack.root, 'fakeclaude-state', 'events.jsonl'), 'utf8')
  const sha = createHash('sha256').update(stack.loginCode).digest('hex')
  expect(evs).toContain(`"sha256":"${sha}","len":${stack.loginCode.length},"verdict":"accepted"`)

  // The sweep: every file the stack wrote — Caddy's log, Drydock's log, the
  // database and its WAL, the stand-in docker's argv log — every URL any
  // request carried, and the page as it stands.
  const [code, state] = stack.loginCode.split('#') as [string, string]
  const written = files(stack.root)
  expect(written.some((f) => f.endsWith('drydock.db'))).toBe(true) // control: the sweep sees the store
  for (const part of [stack.loginCode, code, state]) {
    for (const f of written) {
      expect(readFileSync(f).includes(part), `${f} holds the code`).toBe(false)
    }
    for (const s of stack.apiTap.seen) expect(s.path).not.toContain(part)
    expect(page.url()).not.toContain(part)
    expect(await page.content()).not.toContain(part)
    // A field's value is a property, which page.content() does not show.
    const values = await page.locator('input, textarea').evaluateAll((els) => els.map((e) => (e as HTMLInputElement).value))
    expect(values.join('\n')).not.toContain(part)
  }
})
