import { defineConfig } from 'vitest/config'

// The extension-host tier (vscode-extension §12.5): the pinned VS Code,
// headless, running inner/suite.ts against the client tier's stack. Run by
// test/extension/run.sh host (under xvfb-run when there is no display). The
// CA is made before the workers fork, as for the client tier, and VS Code
// inherits NODE_EXTRA_CA_CERTS from them — which is Spike 05 #1's question.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['test/host/**/*.spec.ts'],
    globalSetup: ['test/client/global.ts'],
    pool: 'forks',
    fileParallelism: false,
    testTimeout: 900_000,
    hookTimeout: 300_000,
  },
})
