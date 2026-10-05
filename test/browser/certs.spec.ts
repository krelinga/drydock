// Certificate validation stays on (testing §10.1, Spike 04 result 3).
//
// The reason ignoreHTTPSErrors is banned is not cookie semantics — Spike 04
// measured that it passes every cookie assertion. It is that it makes the tier
// blind to a certificate served for the wrong host, which is a plausible
// Caddyfile regression. So that blindness is asserted absent, here, against
// the shipped Caddyfile.

import { CaddyProc, presentedCert, UI, UI_HOST } from './harness'
import { expect, test } from './fixtures'
import path from 'node:path'

test('the UI loads over HTTPS from a CA trusted through NSS, and only through NSS', async ({ page, stack }) => {
  // Positive: the trusted browser loads the real app over real HTTPS.
  const resp = await page.goto(`${UI}/signin`)
  expect(resp?.status()).toBe(200)
  expect(await page.evaluate(() => window.isSecureContext)).toBe(true)
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()

  // Control: the same Caddy, the same certificate, a browser whose NSS store
  // lacks the CA. If this loaded, something other than the trust store would
  // be vouching for the certificate and every other test would prove less.
  const untrusted = await stack.launch(stack.caddy.httpsPort, stack.untrustedHome())
  try {
    const p = await untrusted.newPage()
    await expect(p.goto(`${UI}/signin`)).rejects.toThrow(/ERR_CERT_AUTHORITY_INVALID/)
  } finally {
    await untrusted.close()
  }
})

test('a certificate validly signed for the wrong host is refused', async ({ page, stack }) => {
  // The shipped Caddyfile, serving the UI host the preview wildcard — signed
  // by the same trusted CA, and not valid for drydock.test.
  const wrong = await CaddyProc.start(stack, 'wrongcert', {
    uiCert: { cert: path.join(stack.certs, 'preview.crt'), key: path.join(stack.certs, 'preview.key') },
  })
  const b = await stack.launch(wrong.httpsPort, stack.trustedHome)
  try {
    // It really is serving the misissued certificate, from the trusted CA:
    // so a refusal below is about the name, not about trust or liveness.
    const cert = await presentedCert(wrong.httpsPort, UI_HOST)
    expect(cert.subjectaltname).not.toContain(`DNS:${UI_HOST}`)
    expect(cert.issuer.CN).toBe('Drydock browser-tier throwaway CA')

    const p = await b.newPage()
    await expect(p.goto(`${UI}/signin`)).rejects.toThrow(/ERR_CERT_COMMON_NAME_INVALID/)
    // Nothing reached Drydock through the misissued certificate.
    expect(stack.apiTap.seen.filter((s) => s.path.startsWith('/signin'))).toHaveLength(0)

    // Positive control, same browser profile and trust store: the correctly
    // issued certificate on the main Caddy loads.
    const ok = await page.goto(`${UI}/signin`)
    expect(ok?.status()).toBe(200)
  } finally {
    await b.close()
    await wrong.stop()
  }
})
