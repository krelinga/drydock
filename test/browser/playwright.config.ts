// The browser tier (testing §10). Run it with test/browser/run.sh, which
// checks the prerequisites and lets these files resolve @playwright/test from
// web/node_modules — the repository's one Node toolchain (testing §10.1).
import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  // One Stack — one Drydock, one Caddy, one Chromium — shared by every test,
  // so one worker, in file order.
  workers: 1,
  fullyParallel: false,
  // A retry would hide exactly the flakiness this tier exists to notice.
  retries: 0,
  forbidOnly: !!process.env.CI,
  timeout: 60_000,
  reporter: process.env.CI ? [['list'], ['github']] : 'list',
  outputDir: 'test-results',
  use: {
    trace: 'retain-on-failure',
    // Never set ignoreHTTPSErrors (testing §10.1, Spike 04): it would make the
    // tier blind to a certificate served for the wrong host. certs.spec.ts
    // asserts that the browser still notices one.
    ignoreHTTPSErrors: false,
  },
  // Chromium runs every spec against the TLS stack. Firefox and WebKit run
  // engines.spec.ts, against the loopback front: v0.2.1 sent `Origin: null`
  // from Safari, and only another engine could have shown it (testing §10.1).
  // They cannot run the TLS specs, because this tier has no per-run way to
  // make them trust its CA, and ignoring certificate errors is not one.
  projects: [
    { name: 'chromium', use: { browserName: 'chromium' } },
    { name: 'firefox', use: { browserName: 'firefox' }, testMatch: 'engines.spec.ts' },
    { name: 'webkit', use: { browserName: 'webkit' }, testMatch: 'engines.spec.ts' },
  ],
})
