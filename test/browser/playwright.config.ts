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
})
